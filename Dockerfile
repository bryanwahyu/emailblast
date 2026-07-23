# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.25-alpine AS build
WORKDIR /src

# Cache deps first.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# BUILD_TAGS="ses" to compile the Amazon SES backend in. Default = mock+smtp,
# no runtime AWS dependency. CGO off for a static binary.
ARG BUILD_TAGS=""
RUN CGO_ENABLED=0 go build -tags "${BUILD_TAGS}" -ldflags="-s -w" -o /out/emailblast .

# ---- runtime ----
FROM alpine:3.20
# CA certs needed for TLS to SES / SMTP-over-STARTTLS.
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
USER app
WORKDIR /app

COPY --from=build /out/emailblast /usr/local/bin/emailblast

# Recipient list + state are mounted at runtime (see docker-compose.yml).
ENTRYPOINT ["emailblast"]
CMD ["-in", "/data/users.csv", "-checkpoint", "/data/checkpoint.log", "-dlq", "/data/dead-letter.log", "-log-format", "json"]
