package contract

import (
	"embed"
	"io/fs"
)

//go:embed v1/*.schema.json
var schemaFiles embed.FS

// Schema file names in contract/v1/ (INTEGRATION-SPEC §2).
const (
	InputSchema   = "input.schema.json"
	ResultSchema  = "result.schema.json"
	RecordsSchema = "records.schema.json"
	TasksSchema   = "tasks.schema.json"
)

// Schemas returns the embedded JSON Schemas of contract version 1, by file
// name (for example InputSchema).
func Schemas() fs.FS {
	sub, err := fs.Sub(schemaFiles, "v1")
	if err != nil {
		panic(err) // the embed pattern guarantees v1 exists
	}
	return sub
}

// Schema returns one embedded schema by file name.
func Schema(name string) ([]byte, error) {
	return fs.ReadFile(Schemas(), name)
}
