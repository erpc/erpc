package test

import "reflect"

// MergeStructs merges two structs of the same type.
func MergeStructs[T any](s1, s2 T) T {
	v1 := reflect.ValueOf(&s1).Elem()
	v2 := reflect.ValueOf(&s2).Elem()
	mergeValues(v1, v2)
	return s1
}

// mergeValues merges two reflect.Values recursively.
func mergeValues(v1, v2 reflect.Value) {
	for i := 0; i < v1.NumField(); i++ {
		field1 := v1.Field(i)
		field2 := v2.Field(i)

		// If the field from v2 is zero, skip it
		if isZero(field2) {
			continue
		}

		switch field1.Kind() {
		case reflect.Struct:
			mergeValues(field1, field2)
		case reflect.Slice:
			field1.Set(field2)
		default:
			field1.Set(field2)
		}
	}
}

// isZero checks if a reflect.Value is zero.
func isZero(v reflect.Value) bool {
	return reflect.DeepEqual(v.Interface(), reflect.Zero(v.Type()).Interface())
}
