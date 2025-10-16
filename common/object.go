package common

import (
	"fmt"
	"reflect"
	"strings"
	"time"

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

	// Handle special case for Duration
	if fieldValue.Type() == reflect.TypeOf(Duration(0)) {
		jsType := jsValue.ExportType().Kind()
		// Handle string durations like "5s" or "1m"
		if jsType == reflect.String {
			duration, err := time.ParseDuration(jsValue.String())
			if err != nil {
				return fmt.Errorf("invalid duration format: %v", err)
			}
			fieldValue.Set(reflect.ValueOf(Duration(duration)))
			return nil
		}
		// Handle numeric values as milliseconds
		if jsType == reflect.Int || jsType == reflect.Int8 || jsType == reflect.Int16 || jsType == reflect.Int32 || jsType == reflect.Int64 {
			ms := jsValue.ToInteger()
			duration := time.Duration(ms) * time.Millisecond
			fieldValue.Set(reflect.ValueOf(Duration(duration)))
			return nil
		}
	}

	// Handle special case for RateLimitPeriod (accept string or number)
	if fieldValue.Type() == reflect.TypeOf(RateLimitPeriod(0)) {
		jsKind := jsValue.ExportType().Kind()
		switch jsKind {
		case reflect.String:
			s := strings.ToLower(strings.TrimSpace(jsValue.String()))
			var val RateLimitPeriod
			switch s {
			case "second", "1s":
				val = RateLimitPeriodSecond
			case "minute", "1m", "60s":
				val = RateLimitPeriodMinute
			case "hour", "1h", "3600s":
				val = RateLimitPeriodHour
			case "day", "24h", "1d", "86400s":
				val = RateLimitPeriodDay
			case "week", "7d", "168h":
				val = RateLimitPeriodWeek
			case "month", "30d", "720h":
				val = RateLimitPeriodMonth
			case "year", "365d", "8760h":
				val = RateLimitPeriodYear
			default:
				return fmt.Errorf("invalid rate limit period: %s", s)
			}
			fieldValue.Set(reflect.ValueOf(val))
			return nil
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			i := jsValue.ToInteger()
			v := RateLimitPeriod(i)
			switch v {
			case RateLimitPeriodSecond, RateLimitPeriodMinute, RateLimitPeriodHour, RateLimitPeriodDay,
				RateLimitPeriodWeek, RateLimitPeriodMonth, RateLimitPeriodYear:
				fieldValue.Set(reflect.ValueOf(v))
				return nil
			default:
				return fmt.Errorf("invalid rate limit period: %d", i)
			}
		default:
			return fmt.Errorf("invalid rate limit period type: %v", jsKind)
		}
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
	case reflect.Map:
		if jsValue == sobek.Undefined() || jsValue == sobek.Null() {
			return nil
		}

		jsObj, ok := jsValue.(*sobek.Object)
		if !ok {
			return fmt.Errorf("expected object but got %v", jsValue)
		}

		// Create a new map if it's nil
		if fieldValue.IsNil() {
			fieldValue.Set(reflect.MakeMap(fieldValue.Type()))
		}

		// Get the key and value types for the map
		mapKeyType := fieldValue.Type().Key()
		mapValueType := fieldValue.Type().Elem()

		// Get all keys from the JavaScript object
		keys := jsObj.Keys()

		for _, key := range keys {
			jsMapValue := jsObj.Get(key)
			if jsMapValue == sobek.Undefined() {
				continue
			}

			// Create and set the map key (assuming string keys for now)
			mapKey := reflect.New(mapKeyType).Elem()
			if mapKeyType.Kind() == reflect.String {
				mapKey.SetString(key)
			} else {
				return fmt.Errorf("unsupported map key type: %v", mapKeyType.Kind())
			}

			// Create and set the map value
			mapValue := reflect.New(mapValueType).Elem()
			err := setJsValueToGoField(jsMapValue, mapValue)
			if err != nil {
				return fmt.Errorf("failed to set map value for key %s: %v", key, err)
			}

			fieldValue.SetMapIndex(mapKey, mapValue)
		}
	case reflect.Interface:
		fieldValue.Set(reflect.ValueOf(jsValue.Export()))
	default:
		return fmt.Errorf("unsupported kind: %v", fieldValue.Kind())
	}
	return nil
}
