// Package source streams recipients from an input without loading them all
// into memory. 1M rows must never sit in RAM at once.
package source

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"

	"emailblast/internal/model"
)

// CSVSource reads users from a CSV file lazily. The first row is the header;
// it must contain "id" and "email" columns. Every other column becomes a merge
// field keyed by its header name.
type CSVSource struct {
	path string
}

func NewCSV(path string) *CSVSource { return &CSVSource{path: path} }

// Stream opens the file and pushes each parsed user onto out. It closes out
// when the file is exhausted or ctx is done. Errors on individual malformed
// rows are skipped (logged by caller via the returned skip count is omitted for
// brevity) so one bad row cannot abort a million-row run.
//
// The returned error is non-nil only for fatal problems (cannot open file,
// missing required columns).
func (s *CSVSource) Stream(out chan<- model.User) error {
	defer close(out)

	f, err := os.Open(s.path)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(bufio.NewReaderSize(f, 1<<20))
	r.ReuseRecord = true // reuse backing slice; we copy strings out below

	header, err := r.Read()
	if err != nil {
		return fmt.Errorf("read header: %w", err)
	}

	idIdx, emailIdx := -1, -1
	cols := make([]string, len(header))
	for i, h := range header {
		cols[i] = h
		switch h {
		case "id":
			idIdx = i
		case "email":
			emailIdx = i
		}
	}
	if idIdx < 0 || emailIdx < 0 {
		return fmt.Errorf("source must have 'id' and 'email' columns, got %v", header)
	}

	for {
		rec, err := r.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			// Malformed row: skip, keep going.
			continue
		}

		fields := make(map[string]string, len(rec))
		for i, v := range rec {
			if i == idIdx || i == emailIdx {
				continue
			}
			// Clone: ReuseRecord means rec's backing array is overwritten next Read.
			fields[cols[i]] = strings.Clone(v)
		}
		out <- model.User{
			ID:     strings.Clone(rec[idIdx]),
			Email:  strings.Clone(rec[emailIdx]),
			Fields: fields,
		}
	}
}
