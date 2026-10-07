package main

import (
	"database/sql"
	"encoding/json"
	"io"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func rawDB(dsn string) (*sql.DB, error) { return sql.Open("pgx", dsn) }

func jsonNewDecoder(r io.Reader) *json.Decoder { return json.NewDecoder(r) }
