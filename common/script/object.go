package script

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/grafana/sobek"
)

func MapJavascriptObjectToGo(v sobek.Value, dest interface{}) error {
	destVal := reflect.ValueOf(dest)
	if destVal.Kind() != reflect.Ptr || destVal.IsNil() {
		return fmt.Errorf("dest must be a non-nil pointer")
	}

	destElem := destVal.Elem()
	destType := destElem.Type()

	if destElem.Kind() != reflect.Struct {
		return fmt.Errorf("dest must point to a struct")
	}

	if v == nil || v == sobek.Undefined() || v == sobek.Null() {
		return nil
	}

	obj := v.(*sobek.Object)
	if obj == nil {
		return fmt.Errorf("value is not an object")
	}

	for i := 0; i < destType.NumField(); i++ {
		field := destType.Field(i)
		fieldName := field.Name

		jsonTag := field.Tag.Get("json")
		if jsonTag != "" {
			fieldName = strings.Split(jsonTag, ",")[0]
		}

		jsValue := obj.Get(fieldName)
		if jsValue == nil || jsValue == sobek.Undefined() {
			continue
		}

		fieldValue := destElem.Field(i)
		if !fieldValue.CanSet() {
			continue
		}

		err := setJsValueToGoField(jsValue, fieldValue)
		if err != nil {
			return fmt.Errorf("failed to set field %s: %v", fieldName, err)
		}
	}

	return nil
}

func setJsValueToGoField(jsValue sobek.Value, fieldValue reflect.Value) error {
	if !fieldValue.CanSet() {
		return nil
	}

	switch fieldValue.Kind() {
	case reflect.Bool:
		fieldValue.SetBool(jsValue.ToBoolean())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		intVal := jsValue.ToInteger()
		fieldValue.SetInt(intVal)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		intVal := jsValue.ToInteger()
		fieldValue.SetUint(uint64(intVal)) // #nosec G115
	case reflect.Float32, reflect.Float64:
		floatVal := jsValue.ToFloat()
		fieldValue.SetFloat(floatVal)
	case reflect.String:
		strVal := jsValue.String()
		fieldValue.SetString(strVal)
	case reflect.Struct:
		// If the field is of type sobek.Value
		if fieldValue.Type() == reflect.TypeOf(sobek.Value(nil)) {
			fieldValue.Set(reflect.ValueOf(jsValue))
		} else {
			return MapJavascriptObjectToGo(jsValue, fieldValue.Addr().Interface())
		}
	case reflect.Ptr:
		if jsValue == sobek.Undefined() || jsValue == sobek.Null() {
			return nil
		}
		ptrValue := reflect.New(fieldValue.Type().Elem())
		err := setJsValueToGoField(jsValue, ptrValue.Elem())
		if err != nil {
			return err
		}
		fieldValue.Set(ptrValue)
	case reflect.Slice:
		if jsValue == sobek.Undefined() || jsValue == sobek.Null() {
			return nil
		}
		if jsValue.ExportType().Kind() != reflect.Slice {
			return fmt.Errorf("expected array but got %v", jsValue)
		}
		jsArray := jsValue.(*sobek.Object)
		length := jsArray.Get("length").ToInteger()
		slice := reflect.MakeSlice(fieldValue.Type(), int(length), int(length))
		for i := 0; i < int(length); i++ {
			elemValue := jsArray.Get(fmt.Sprintf("%d", i))
			if elemValue == sobek.Undefined() {
				return fmt.Errorf("expected array element %d to be a sobek.Value", i)
			}
			err := setJsValueToGoField(elemValue, slice.Index(i))
			if err != nil {
				return err
			}
		}
		fieldValue.Set(slice)
	case reflect.Func:
		if obj, ok := jsValue.(*sobek.Object); ok {
			if fn, ok := sobek.AssertFunction(obj); ok {
				fieldValue.Set(reflect.ValueOf(fn))
			} else {
				return fmt.Errorf("field is not a function")
			}
		} else {
			return fmt.Errorf("field is not a function")
		}
	case reflect.Interface:
		fieldValue.Set(reflect.ValueOf(jsValue.Export()))
	default:
		return fmt.Errorf("unsupported kind: %v", fieldValue.Kind())
	}
	return nil
}
