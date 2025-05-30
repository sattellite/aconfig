package aconfig

import (
	"encoding"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

type fieldData struct {
	name       string
	parent     *fieldData
	field      reflect.StructField
	value      reflect.Value
	isSet      bool
	isRequired bool
	tags       map[string]string
}

func (f *fieldData) Name() string {
	return f.name
}

func (f *fieldData) Tag(tag string) string {
	if t, ok := f.tags[tag]; ok {
		return t
	}
	return f.field.Tag.Get(tag)
}

func (f *fieldData) Parent() (Field, bool) {
	return f.parent, f.parent != nil
}

func (l *Loader) newSimpleFieldData(value reflect.Value) *fieldData {
	return l.newFieldData(reflect.StructField{}, value, nil)
}

func (l *Loader) newFieldData(field reflect.StructField, value reflect.Value, parent *fieldData) *fieldData {
	requiredTag := field.Tag.Get("required")
	if requiredTag != "" && requiredTag != "true" {
		panic(fmt.Sprintf("aconfig: incorrect value for 'required' tag: %v", requiredTag))
	}

	fd := &fieldData{
		name:       makeName(field.Name, parent),
		parent:     parent,
		value:      value,
		field:      field,
		isSet:      false,
		isRequired: requiredTag == "true",
		tags:       l.tagsForField(field),
	}
	return fd
}

func (l *Loader) tagsForField(field reflect.StructField) map[string]string {
	words := splitNameByWords(field.Name)

	tags := map[string]string{
		"default": field.Tag.Get("default"),
		"usage":   field.Tag.Get("usage"),

		"env":  l.makeTagValue(field, "env", words),
		"flag": l.makeTagValue(field, "flag", words),
	}

	for _, dec := range l.config.FileDecoders {
		tags[dec.Format()] = l.makeTagValue(field, dec.Format(), words)
	}
	return tags
}

func (l *Loader) fullTag(prefix string, f *fieldData, tag string) string {
	sep := "."
	if tag == "flag" {
		sep = l.config.FlagDelimiter
	}
	if tag == "env" {
		sep = l.config.envDelimiter
	}
	res := f.Tag(tag)
	if res == "-" {
		return ""
	}
	if before, _, ok := cut(res, ",exact"); ok {
		return before
	}
	if before, _, ok := cut(res, ",omitempty"); ok {
		return before
	}
	for p := f.parent; p != nil; p = p.parent {
		if p.Tag(tag) != "-" {
			res = p.Tag(tag) + sep + res
		}
	}
	return prefix + res
}

func (l *Loader) getFields(x interface{}) []*fieldData {
	value := reflect.ValueOf(x)
	for value.Type().Kind() == reflect.Ptr {
		value = value.Elem()
	}
	return l.getFieldsHelper(value, nil)
}

func (l *Loader) getFieldsHelper(valueObject reflect.Value, parent *fieldData) []*fieldData {
	typeObject := valueObject.Type()
	count := valueObject.NumField()

	fields := make([]*fieldData, 0, count)
	for i := 0; i < count; i++ {
		value := valueObject.Field(i)
		field := typeObject.Field(i)

		if !value.CanSet() {
			continue
		}

		fd := l.newFieldData(field, value, parent)

		// if it's a struct - expand and process it's fields
		kind := field.Type.Kind()
		if kind == reflect.Ptr {
			kind = field.Type.Elem().Kind()
		}
		if kind == reflect.Struct {
			var subFieldParent *fieldData
			if field.Anonymous {
				subFieldParent = parent
			} else {
				subFieldParent = fd
			}
			if field.Type.Kind() == reflect.Ptr {
				value.Set(reflect.New(field.Type.Elem()))
				value = value.Elem()
			}
			fields = append(fields, l.getFieldsHelper(value, subFieldParent)...)
			continue
		}
		fields = append(fields, fd)
	}
	return fields
}

func (l *Loader) setFieldData(field *fieldData, value interface{}) error {
	if value == nil {
		return nil
	}

	// unwrap pointers
	for field.value.Type().Kind() == reflect.Ptr {
		if field.value.IsNil() {
			field.value.Set(reflect.New(field.value.Type().Elem()))
		}
		field.value = field.value.Elem()
	}

	if value == "" {
		return nil
	}

	if field.value.CanAddr() {
		pv := field.value.Addr().Interface()
		if v, ok := pv.(encoding.TextUnmarshaler); ok {
			return v.UnmarshalText([]byte(fmt.Sprint(value)))
		}
	}

	switch kind := field.value.Type().Kind(); kind {
	case reflect.Bool:
		return l.setBool(field, fmt.Sprint(value))

	case reflect.String:
		return l.setString(field, fmt.Sprint(value))

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32:
		return l.setInt(field, fmt.Sprint(value))

	case reflect.Int64:
		return l.setInt64(field, fmt.Sprint(value))

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return l.setUint(field, fmt.Sprint(value))

	case reflect.Float32, reflect.Float64:
		return l.setFloat(field, fmt.Sprint(value))

	case reflect.Interface:
		return l.setInterface(field, value)

	case reflect.Struct:
		fd := l.newFieldData(reflect.StructField{}, field.value, nil)
		return l.m2s(mii(value), fd.value)

	case reflect.Slice:
		if isPrimitive(field.field.Type.Elem()) {
			return l.setSlice(field, l.sliceToString(value))
		}

		in := reflect.ValueOf(value)
		if in.Kind() != reflect.Slice {
			panic(fmt.Errorf("%T %v", value, value))
		}

		out := reflect.MakeSlice(field.field.Type, in.Len(), in.Len())
		field.value.Set(out)

		for i := 0; i < in.Len(); i++ {
			fd := l.newFieldData(reflect.StructField{}, out.Index(i), nil)

			if err := l.setFieldData(fd, in.Index(i).Interface()); err != nil {
				return err
			}
		}

		return nil

	case reflect.Map:
		v, ok := value.(map[string]interface{})
		if !ok {
			return l.setMap(field, fmt.Sprint(value))
		}

		mapp := reflect.MakeMapWithSize(field.field.Type, len(v))
		for key, val := range v {
			fdk := l.newSimpleFieldData(reflect.New(field.field.Type.Key()).Elem())
			if err := l.setFieldData(fdk, key); err != nil {
				return fmt.Errorf("incorrect map key %q: %w", key, err)
			}

			fdv := l.newFieldData(reflect.StructField{}, reflect.New(field.value.Type().Elem()).Elem(), field)
			fdv.field.Type = field.value.Type().Elem()
			if err := l.setFieldData(fdv, val); err != nil {
				return fmt.Errorf("incorrect map value %q: %w", val, err)
			}

			mapp.SetMapIndex(fdk.value, fdv.value)
		}
		field.value.Set(mapp)
		return nil

	default:
		return fmt.Errorf("type kind %q isn't supported", kind)
	}
}

func (*Loader) setBool(field *fieldData, value string) error {
	val, err := strconv.ParseBool(value)
	if err != nil {
		return err
	}
	field.value.SetBool(val)
	return nil
}

func (*Loader) setInt(field *fieldData, value string) error {
	val, err := strconv.ParseInt(value, 0, field.value.Type().Bits())
	if err != nil {
		return err
	}
	field.value.SetInt(val)
	return nil
}

func (l *Loader) setInt64(field *fieldData, value string) error {
	if field.field.Type == reflect.TypeOf(time.Second) {
		val, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		field.value.Set(reflect.ValueOf(val))
		return nil
	}
	return l.setInt(field, value)
}

func (*Loader) setUint(field *fieldData, value string) error {
	val, err := strconv.ParseUint(value, 0, field.value.Type().Bits())
	if err != nil {
		return err
	}
	field.value.SetUint(val)
	return nil
}

func (*Loader) setFloat(field *fieldData, value string) error {
	val, err := strconv.ParseFloat(value, field.value.Type().Bits())
	if err != nil {
		return err
	}
	field.value.SetFloat(val)
	return nil
}

func (*Loader) setString(field *fieldData, value string) error {
	field.value.SetString(value)
	return nil
}

func (*Loader) setInterface(field *fieldData, value interface{}) error {
	field.value.Set(reflect.ValueOf(value))
	return nil
}

func (l *Loader) setSlice(field *fieldData, value string) error {
	// Special case for []byte
	if field.field.Type.Elem().Kind() == reflect.Uint8 {
		value := reflect.ValueOf([]byte(value))
		field.value.Set(value)
		return nil
	}

	vals := strings.Split(value, l.config.SliceSeparator)
	slice := reflect.MakeSlice(field.field.Type, len(vals), len(vals))
	for i, val := range vals {
		val = strings.TrimSpace(val)

		fd := l.newFieldData(reflect.StructField{}, slice.Index(i), nil)
		fd.field.Type = field.field.Type.Elem()
		if err := l.setFieldData(fd, val); err != nil {
			return fmt.Errorf("incorrect slice item %q: %w", val, err)
		}
	}
	field.value.Set(slice)
	return nil
}

func (l *Loader) setMap(field *fieldData, value string) error {
	vals := strings.Split(value, ",")
	mapField := reflect.MakeMapWithSize(field.field.Type, len(vals))

	for _, val := range vals {
		entry := strings.SplitN(val, ":", 2)
		if len(entry) != 2 {
			return fmt.Errorf("incorrect map item: %s", val)
		}
		key := strings.TrimSpace(entry[0])
		val := strings.TrimSpace(entry[1])

		fdk := l.newSimpleFieldData(reflect.New(field.field.Type.Key()).Elem())
		if err := l.setFieldData(fdk, key); err != nil {
			return fmt.Errorf("incorrect map key %q: %w", key, err)
		}

		fdv := l.newFieldData(reflect.StructField{}, reflect.New(field.value.Type().Elem()).Elem(), field)
		fdv.field.Type = field.value.Type().Elem()
		if err := l.setFieldData(fdv, val); err != nil {
			return fmt.Errorf("incorrect map value %q: %w", val, err)
		}
		mapField.SetMapIndex(fdk.value, fdv.value)
	}
	field.value.Set(mapField)
	return nil
}

func (l *Loader) m2s(m map[string]interface{}, structValue reflect.Value) error {
	for name, value := range m {
		name = strings.Title(name)
		structFieldValue := structValue.FieldByName(name)
		if !structFieldValue.IsValid() {
			return fmt.Errorf("no such field %q in struct", name)
		}

		if !structFieldValue.CanSet() {
			return fmt.Errorf("cannot set %q field value", name)
		}

		field, _ := structValue.Type().FieldByName(name)

		fd := l.newFieldData(field, structFieldValue, nil)
		if err := l.setFieldData(fd, value); err != nil {
			return err
		}
	}
	return nil
}

func mii(m interface{}) map[string]interface{} {
	switch m := m.(type) {
	case map[string]interface{}:
		return m
	case map[interface{}]interface{}:
		res := map[string]interface{}{}
		for k, v := range m {
			res[k.(string)] = v
		}
		return res
	default:
		panic(fmt.Sprintf("%T %v", m, m))
	}
}
