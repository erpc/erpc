package common

import (
	"fmt"
	"reflect"
	"strings"
)

// configPackagePath limits the walk below to config structs in this package.
// Other types (sobek programs, sync primitives, stdlib types) are left alone.
var configPackagePath = reflect.TypeOf(Config{}).PkgPath()

// maxConfigWalkDepth stops the walk on a config type that points back at
// itself. The real config tree is far shallower; the cap only bounds a cycle.
const maxConfigWalkDepth = 256

// rejectEmptyConfigItems refuses a config that holds an empty item in a list
// or map of config objects, and names the item.
//
// YAML decodes a list item with nothing after the dash ("upstreams:\n  -"),
// or a map key with no value, into a nil pointer. JSON does the same for a
// literal null. SetDefaults dereferences those pointers, so one stray dash
// made eRPC panic at boot with no hint of which list held it.
//
// The walk is generic, so it also covers lists added later. An empty object
// written as {} is not nil and passes.
func rejectEmptyConfigItems(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	return walkForEmptyConfigItems(reflect.ValueOf(cfg), "", 0)
}

func walkForEmptyConfigItems(v reflect.Value, path string, depth int) error {
	if !v.IsValid() || depth > maxConfigWalkDepth {
		return nil
	}

	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return walkForEmptyConfigItems(v.Elem(), path, depth+1)

	case reflect.Struct:
		t := v.Type()
		if t.PkgPath() != configPackagePath {
			return nil
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			if err := walkForEmptyConfigItems(v.Field(i), joinConfigPath(path, configFieldName(f)), depth+1); err != nil {
				return err
			}
		}

	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			itemPath := fmt.Sprintf("%s[%d]", path, i)
			if isEmptyConfigItem(v.Index(i)) {
				return emptyConfigItemError(itemPath)
			}
			if err := walkForEmptyConfigItems(v.Index(i), itemPath, depth+1); err != nil {
				return err
			}
		}

	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			itemPath := fmt.Sprintf("%s[%v]", path, iter.Key())
			if isEmptyConfigItem(iter.Value()) {
				return emptyConfigItemError(itemPath)
			}
			if err := walkForEmptyConfigItems(iter.Value(), itemPath, depth+1); err != nil {
				return err
			}
		}
	}

	return nil
}

// isEmptyConfigItem reports whether a list or map element is a nil pointer to
// a struct. A nil inside an []interface{} (for example cache policy params) is
// a JSON-RPC null, not an empty item, and passes.
func isEmptyConfigItem(v reflect.Value) bool {
	return v.Kind() == reflect.Pointer && v.IsNil() && v.Type().Elem().Kind() == reflect.Struct
}

func emptyConfigItemError(path string) error {
	return fmt.Errorf("config: %s is empty, remove the entry or fill it in (write {} for an empty object)", path)
}

// configFieldName returns the YAML name of a field, or "" for an inline field,
// so the path matches what the operator wrote.
func configFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("yaml")
	name, opts, _ := strings.Cut(tag, ",")
	if name == "" && strings.Contains(","+opts+",", ",inline,") {
		return ""
	}
	if name == "" || name == "-" {
		return f.Name
	}
	return name
}

func joinConfigPath(parent, field string) string {
	if field == "" {
		return parent
	}
	if parent == "" {
		return field
	}
	return parent + "." + field
}
