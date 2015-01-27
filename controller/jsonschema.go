package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/cupcake/jsonschema"
	ct "github.com/flynn/flynn/controller/types"
)

var schemaCache map[string]*jsonschema.Schema

func loadSchemas() error {
	if schemaCache != nil {
		return nil
	}

	var schemaPaths []string
	walkFn := func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() {
			schemaPaths = append(schemaPaths, path)
		}
		return nil
	}
	schemaRoot, err := filepath.Abs(filepath.Join("..", "website", "schema"))
	if err != nil {
		return err
	}
	filepath.Walk(schemaRoot, walkFn)

	schemaCache = make(map[string]*jsonschema.Schema, len(schemaPaths))
	for _, path := range schemaPaths {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		schema := &jsonschema.Schema{Cache: schemaCache}
		err = schema.ParseWithoutRefs(file)
		if err != nil {
			return err
		}
		cacheKey := "https://flynn.io/schema" + strings.TrimSuffix(filepath.Base(path), ".json")
		schemaCache[cacheKey] = schema
		file.Close()
	}
	for _, schema := range schemaCache {
		schema.ResolveRefs(false)
	}

	return nil
}

func schemaForType(thing interface{}) *jsonschema.Schema {
	name := strings.ToLower(reflect.Indirect(reflect.ValueOf(thing)).Type().Name())
	if name == "newjob" {
		name = "new_job"
	}
	if name == "appupdate" {
		name = "app"
	}
	if name == "route" {
		return schemaCache["https://flynn.io/schema/router/route"]
	}
	cacheKey := "https://flynn.io/schema/controller/" + name
	return schemaCache[cacheKey]
}

func schemaValidate(thing interface{}) error {
	schema := schemaForType(thing)
	if schema == nil {
		return errors.New("Unknown resource")
	}

	var validateData map[string]interface{}
	var data []byte
	var err error
	if data, err = json.Marshal(thing); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&validateData); err != nil {
		return err
	}

	schemaErrs := schema.Validate(nil, validateData)
	if len(schemaErrs) > 0 {
		err := schemaErrs[0]
		return ct.ValidationError{
			Message: err.Description,
			Field:   err.DotNotation(),
		}
	}

	return nil
}
