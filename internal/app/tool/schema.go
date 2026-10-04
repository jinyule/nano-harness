package tool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// maxArgumentDepth bounds nesting while parsing untrusted argument JSON.
const maxArgumentDepth = 64

// Type names a JSON value type accepted by tool parameters.
type Type string

const (
	// TypeString accepts a JSON string.
	TypeString Type = "string"
	// TypeNumber accepts a finite JSON number other than negative zero.
	TypeNumber Type = "number"
	// TypeBoolean accepts true or false.
	TypeBoolean Type = "boolean"
	// TypeArray accepts a JSON array whose elements match Items.
	TypeArray Type = "array"
	// TypeObject accepts a JSON object whose members match Properties.
	TypeObject Type = "object"
)

// Schema describes one argument value in the enforced JSON Schema subset:
// type, description, string enum, array items, and nested object members.
// Keywords outside this subset cannot be declared, so every serialized
// constraint is also validated.
type Schema struct {
	Type        Type
	Description string
	// Enum restricts a string to the listed literals in model-visible order.
	Enum []string
	// Items is the element schema of an array and is required for arrays.
	Items *Schema
	// Properties are the members of a nested object in model-visible order.
	Properties []Property
	// AdditionalProperties admits undeclared members of a nested object. It
	// is serialized for every nested object; the zero value rejects them.
	AdditionalProperties bool
}

// Property is one named object member. A required member must be present;
// JSON null never satisfies a declared type.
type Property struct {
	Name     string
	Schema   Schema
	Required bool
}

// Parameters is the implicit object root of a tool's arguments in
// model-visible order. The serialized root carries only type, properties,
// and required, matching the upstream definition format; validation still
// rejects undeclared root members.
type Parameters []Property

// String declares a string value, optionally restricted to enum literals.
func String(description string, enum ...string) Schema {
	return Schema{Type: TypeString, Description: description, Enum: enum}
}

// Number declares a finite JSON number.
func Number(description string) Schema { return Schema{Type: TypeNumber, Description: description} }

// Boolean declares a JSON boolean.
func Boolean(description string) Schema { return Schema{Type: TypeBoolean, Description: description} }

// Array declares a JSON array whose elements match items.
func Array(description string, items Schema) Schema {
	return Schema{Type: TypeArray, Description: description, Items: &items}
}

// Object declares a nested JSON object with explicit openness.
func Object(description string, additionalProperties bool, properties ...Property) Schema {
	return Schema{Type: TypeObject, Description: description, Properties: properties, AdditionalProperties: additionalProperties}
}

// Required declares a member that must be present.
func Required(name string, schema Schema) Property {
	return Property{Name: name, Schema: schema, Required: true}
}

// Optional declares a member that may be omitted.
func Optional(name string, schema Schema) Property {
	return Property{Name: name, Schema: schema}
}

func (parameters Parameters) check() error {
	return checkProperties(parameters, "parameters")
}

func checkProperties(properties []Property, path string) error {
	seen := map[string]struct{}{}
	for _, property := range properties {
		if property.Name == "" {
			return fmt.Errorf("%s has an unnamed property", path)
		}
		if _, ok := seen[property.Name]; ok {
			return fmt.Errorf("%s declares %q twice", path, property.Name)
		}
		seen[property.Name] = struct{}{}
		if err := property.Schema.check(path + "." + property.Name); err != nil {
			return err
		}
	}
	return nil
}

func (schema Schema) check(path string) error {
	if len(schema.Enum) > 0 && schema.Type != TypeString {
		return fmt.Errorf("%s declares enum on type %q", path, schema.Type)
	}
	if (schema.Items != nil) != (schema.Type == TypeArray) {
		return fmt.Errorf("%s must declare items exactly when it is an array", path)
	}
	if schema.Type != TypeObject && (len(schema.Properties) > 0 || schema.AdditionalProperties) {
		return fmt.Errorf("%s declares object members on type %q", path, schema.Type)
	}
	seen := map[string]struct{}{}
	for _, literal := range schema.Enum {
		if _, ok := seen[literal]; ok {
			return fmt.Errorf("%s repeats enum literal %q", path, literal)
		}
		seen[literal] = struct{}{}
	}
	switch schema.Type {
	case TypeString, TypeNumber, TypeBoolean:
		return nil
	case TypeArray:
		return schema.Items.check(path + "[]")
	case TypeObject:
		return checkProperties(schema.Properties, path)
	default:
		return fmt.Errorf("%s has unsupported type %q", path, schema.Type)
	}
}

// checkGoType proves a decode target has exactly one compatible field per
// declared member, so validated arguments can never be silently dropped.
func checkGoType(target reflect.Type, properties []Property, path string) error {
	if target.Kind() != reflect.Struct {
		return fmt.Errorf("%s must decode into a struct", path)
	}
	fields := map[string]reflect.Type{}
	for field := range target.Fields() {
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			return fmt.Errorf("%s field %s needs a JSON member name", path, field.Name)
		}
		fields[name] = field.Type
	}
	if len(fields) != len(properties) {
		return fmt.Errorf("%s decodes %d fields for %d declared members", path, len(fields), len(properties))
	}
	for _, property := range properties {
		field, ok := fields[property.Name]
		if !ok {
			return fmt.Errorf("%s has no field for %q", path, property.Name)
		}
		if err := checkGoValue(field, property.Schema, path+"."+property.Name); err != nil {
			return err
		}
	}
	return nil
}

func checkGoValue(target reflect.Type, schema Schema, path string) error {
	if target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	want := map[Type]reflect.Kind{
		TypeString: reflect.String, TypeNumber: reflect.Float64, TypeBoolean: reflect.Bool,
		TypeArray: reflect.Slice, TypeObject: reflect.Struct,
	}[schema.Type]
	if target.Kind() != want {
		return fmt.Errorf("%s decodes %s into %s", path, schema.Type, target)
	}
	switch schema.Type {
	case TypeArray:
		return checkGoValue(target.Elem(), *schema.Items, path+"[]")
	case TypeObject:
		return checkGoType(target, schema.Properties, path)
	case TypeString, TypeNumber, TypeBoolean:
	}
	return nil
}

// marshal serializes the root with upstream key order: type, properties,
// then required when any member is required.
func (parameters Parameters) marshal() json.RawMessage {
	var buffer bytes.Buffer
	buffer.WriteString(`{"type":"object",`)
	writeMembers(&buffer, parameters, true)
	buffer.WriteByte('}')
	return buffer.Bytes()
}

func writeMembers(buffer *bytes.Buffer, properties []Property, alwaysProperties bool) {
	if len(properties) > 0 || alwaysProperties {
		buffer.WriteString(`"properties":{`)
		for index, property := range properties {
			if index > 0 {
				buffer.WriteByte(',')
			}
			writeString(buffer, property.Name)
			buffer.WriteByte(':')
			property.Schema.write(buffer)
		}
		buffer.WriteByte('}')
	}
	required := make([]string, 0, len(properties))
	for _, property := range properties {
		if property.Required {
			required = append(required, property.Name)
		}
	}
	if len(required) > 0 {
		buffer.WriteString(`,"required":[`)
		for index, name := range required {
			if index > 0 {
				buffer.WriteByte(',')
			}
			writeString(buffer, name)
		}
		buffer.WriteByte(']')
	}
}

// write emits type, description, then the type-specific keywords in the
// order produced by the upstream schema compiler.
func (schema Schema) write(buffer *bytes.Buffer) {
	buffer.WriteString(`{"type":`)
	writeString(buffer, string(schema.Type))
	if schema.Description != "" {
		buffer.WriteString(`,"description":`)
		writeString(buffer, schema.Description)
	}
	switch schema.Type {
	case TypeString:
		if len(schema.Enum) > 0 {
			buffer.WriteString(`,"enum":[`)
			for index, literal := range schema.Enum {
				if index > 0 {
					buffer.WriteByte(',')
				}
				writeString(buffer, literal)
			}
			buffer.WriteByte(']')
		}
	case TypeArray:
		buffer.WriteString(`,"items":`)
		schema.Items.write(buffer)
	case TypeObject:
		buffer.WriteString(`,"additionalProperties":`)
		buffer.WriteString(strconv.FormatBool(schema.AdditionalProperties))
		if len(schema.Properties) > 0 {
			buffer.WriteByte(',')
			writeMembers(buffer, schema.Properties, false)
		}
	case TypeNumber, TypeBoolean:
	}
	buffer.WriteByte('}')
}

// writeString encodes text like JSON.stringify: no HTML escaping, so
// descriptions containing <, >, or & stay byte-identical to upstream.
func writeString(buffer *bytes.Buffer, text string) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(text) // encoding a Go string cannot fail
	buffer.Write(bytes.TrimSuffix(encoded.Bytes(), []byte("\n")))
}

type jsonKind uint8

const (
	jsonNull jsonKind = iota
	jsonBool
	jsonNumber
	jsonString
	jsonArray
	jsonObject
)

// jsonValue keeps object members in input order so undeclared members are
// reported deterministically.
type jsonValue struct {
	kind    jsonKind
	text    string
	members []jsonMember
	items   []jsonValue
}

type jsonMember struct {
	key   string
	value jsonValue
}

func parseArguments(raw json.RawMessage) (jsonValue, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := parseValue(decoder, 0)
	if err != nil {
		return jsonValue{}, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return jsonValue{}, errors.New("arguments contain a trailing value")
	}
	return value, nil
}

func parseValue(decoder *json.Decoder, depth int) (jsonValue, error) {
	if depth > maxArgumentDepth {
		return jsonValue{}, fmt.Errorf("arguments nest deeper than %d levels", maxArgumentDepth)
	}
	token, err := decoder.Token()
	if err != nil {
		return jsonValue{}, fmt.Errorf("arguments are not valid JSON: %w", err)
	}
	switch typed := token.(type) {
	case nil:
		return jsonValue{kind: jsonNull}, nil
	case bool:
		return jsonValue{kind: jsonBool}, nil
	case json.Number:
		return jsonValue{kind: jsonNumber, text: typed.String()}, nil
	case string:
		return jsonValue{kind: jsonString, text: typed}, nil
	default:
		// A token that is not a scalar is a delimiter; only '{' and '[' can
		// start a value because the decoder rejects misplaced closers.
		if typed == json.Delim('[') {
			value := jsonValue{kind: jsonArray}
			for decoder.More() {
				item, err := parseValue(decoder, depth+1)
				if err != nil {
					return jsonValue{}, err
				}
				value.items = append(value.items, item)
			}
			_, err := decoder.Token()
			return value, err
		}
		value := jsonValue{kind: jsonObject}
		seen := map[string]struct{}{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return jsonValue{}, fmt.Errorf("arguments are not valid JSON: %w", err)
			}
			name, _ := key.(string) // object keys are always strings once More reports a member
			if _, ok := seen[name]; ok {
				return jsonValue{}, fmt.Errorf("arguments repeat property %q", name)
			}
			seen[name] = struct{}{}
			member, err := parseValue(decoder, depth+1)
			if err != nil {
				return jsonValue{}, err
			}
			value.members = append(value.members, jsonMember{key: name, value: member})
		}
		_, err := decoder.Token()
		return value, err
	}
}

// validate collects every violation in schema-walk order: missing required
// members, declared members in declaration order, then undeclared members.
func (parameters Parameters) validate(raw json.RawMessage) error {
	value, err := parseArguments(raw)
	if err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if value.kind != jsonObject {
		return errors.New(`invalid arguments: "arguments" must be an object`)
	}
	var violations []string
	validateObject(parameters, false, value, "", &violations)
	if len(violations) > 0 {
		return fmt.Errorf("invalid arguments: %s", strings.Join(violations, "; "))
	}
	return nil
}

// validateObject checks members of a value already known to be an object.
func validateObject(properties []Property, additional bool, value jsonValue, path string, violations *[]string) {
	present := map[string]jsonValue{}
	for _, member := range value.members {
		present[member.key] = member.value
	}
	for _, property := range properties {
		if _, ok := present[property.Name]; property.Required && !ok {
			*violations = append(*violations, fmt.Sprintf("missing required property %q", memberPath(path, property.Name)))
		}
	}
	declared := map[string]struct{}{}
	for _, property := range properties {
		declared[property.Name] = struct{}{}
		if member, ok := present[property.Name]; ok {
			validateValue(property.Schema, member, memberPath(path, property.Name), violations)
		}
	}
	if additional {
		return
	}
	for _, member := range value.members {
		if _, ok := declared[member.key]; !ok {
			*violations = append(*violations, fmt.Sprintf("%q is not a declared property", memberPath(path, member.key)))
		}
	}
}

func validateValue(schema Schema, value jsonValue, path string, violations *[]string) {
	want := map[Type]jsonKind{
		TypeString: jsonString, TypeNumber: jsonNumber, TypeBoolean: jsonBool,
		TypeArray: jsonArray, TypeObject: jsonObject,
	}[schema.Type]
	if value.kind != want {
		article := "a"
		if schema.Type == TypeArray || schema.Type == TypeObject {
			article = "an"
		}
		*violations = append(*violations, fmt.Sprintf("%q must be %s %s", path, article, schema.Type))
		return
	}
	switch schema.Type {
	case TypeString:
		if len(schema.Enum) > 0 && !slices.Contains(schema.Enum, value.text) {
			encoded, _ := json.Marshal(schema.Enum) // a string slice always encodes
			*violations = append(*violations, fmt.Sprintf("%q must be one of %s", path, encoded))
		}
	case TypeNumber:
		number, err := strconv.ParseFloat(value.text, 64)
		if err != nil || math.IsInf(number, 0) || number == 0 && math.Signbit(number) {
			*violations = append(*violations, fmt.Sprintf("%q must be a finite JSON number", path))
		}
	case TypeArray:
		for index, item := range value.items {
			validateValue(*schema.Items, item, fmt.Sprintf("%s[%d]", path, index), violations)
		}
	case TypeObject:
		validateObject(schema.Properties, schema.AdditionalProperties, value, path, violations)
	case TypeBoolean:
	}
}

func memberPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
