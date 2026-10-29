# gormnul

[![Go](https://github.com/wuhan005/gormnul/actions/workflows/go.yml/badge.svg)](https://github.com/wuhan005/gormnul/actions/workflows/go.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/wuhan005/gormnul.svg)](https://pkg.go.dev/github.com/wuhan005/gormnul)

A GORM plugin that strips NUL (`\x00`) characters from PostgreSQL text and JSON write values.

## TL;DR

PostgreSQL does not accept NUL in `text` or `jsonb`. One stray `\x00` from user input or an upstream API fails the whole write:

```
ERROR: invalid byte sequence for encoding "UTF8": 0x00 (SQLSTATE 22021)
ERROR: unsupported Unicode escape sequence (SQLSTATE 22P05)
```

`gormnul` removes NUL from the values GORM binds for text and JSON columns, so these writes succeed. Binary (`bytea`) data is left untouched.

## Installation

```bash
go get github.com/wuhan005/gormnul
```

## Usage

Register the plugin once on the root `*gorm.DB`, before it is used concurrently:

```go
db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
if err != nil {
	return err
}

if err := db.Use(gormnul.Plugin{}); err != nil {
	return err
}
```

After that, `Create`, `CreateInBatches`, `Save`, `Updates`, `UpdateColumn(s)` and `OnConflict` upserts are cleaned automatically.

## License

MIT License
