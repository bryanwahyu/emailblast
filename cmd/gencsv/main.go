// Command gencsv writes a deterministic test recipient list to stdout (or -out).
// The CSV itself is NOT committed — regenerate it on demand:
//
//	go run ./cmd/gencsv -n 1000000 -out users_1m.csv
//
// Columns match what the sender expects: id, email, first_name, plan.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
)

func main() {
	n := flag.Int("n", 1_000_000, "number of rows")
	out := flag.String("out", "", "output file (default stdout)")
	domain := flag.String("domain", "example.com", "email domain")
	flag.Parse()

	w := bufio.NewWriterSize(os.Stdout, 1<<20)
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintln(os.Stderr, "create:", err)
			os.Exit(1)
		}
		defer f.Close()
		w = bufio.NewWriterSize(f, 1<<20)
	}
	defer w.Flush()

	names := []string{"Alice", "Bob", "Carol", "Dave", "Erin", "Frank", "Grace", "Heidi", "Ivan", "Judy"}
	plans := []string{"Free", "Pro", "Enterprise"}

	fmt.Fprintln(w, "id,email,first_name,plan")
	for i := 0; i < *n; i++ {
		// Deterministic (no RNG) so runs are reproducible and diffable.
		name := names[i%len(names)]
		plan := plans[i%len(plans)]
		fmt.Fprintf(w, "u%08d,user%08d@%s,%s,%s\n", i, i, *domain, name, plan)
	}
}
