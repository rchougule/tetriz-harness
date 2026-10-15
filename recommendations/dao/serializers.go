// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"fmt"
	"reflect"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"gorm.io/gorm/schema"
)

// GORM serializers that bind the model layer's proto types to their DB columns: a proto enum stored
// as its full value-NAME string (text column), and a proto message stored as protojson (JSONB
// column). Some proto plugins generate per-type Value/Scan methods onto the proto types; this engine
// regenerates protos with the standard toolchain (no such methods), so the
// wiring lives here instead, applied via the `serializer:protoenum` / `serializer:protojson` field
// tags on the model structs. Registered once at import — the GORM serializer registry is a process
// global, the same shape a database/sql driver registers itself at import.
func init() {
	schema.RegisterSerializer("protoenum", protoEnumSerializer{})
	schema.RegisterSerializer("protojson", protoJSONSerializer{})
}

// protoEnumSerializer stores a proto enum as its value-name string and parses it back. It handles a
// value enum (e.g. ArtifactType) and a pointer enum (e.g. *ArtifactRecommendationSource); a nil
// pointer or a NULL column round-trips as SQL NULL / the zero value.
type protoEnumSerializer struct{}

func (protoEnumSerializer) Value(_ context.Context, field *schema.Field, _ reflect.Value, fieldValue interface{}) (interface{}, error) {
	if fieldValue == nil {
		return nil, nil
	}
	rv := reflect.ValueOf(fieldValue)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, nil
		}
		rv = rv.Elem()
	}
	enum, ok := rv.Interface().(protoreflect.Enum)
	if !ok {
		return nil, fmt.Errorf("protoenum serializer: field %s value %T is not a proto enum", field.Name, fieldValue)
	}
	descriptor := enum.Descriptor().Values().ByNumber(enum.Number())
	if descriptor == nil {
		return nil, fmt.Errorf("protoenum serializer: field %s carries unknown enum number %d", field.Name, enum.Number())
	}
	return string(descriptor.Name()), nil
}

func (protoEnumSerializer) Scan(ctx context.Context, field *schema.Field, dst reflect.Value, dbValue interface{}) error {
	target := field.ReflectValueOf(ctx, dst)
	if dbValue == nil {
		return nil // leave the zero value (or nil pointer)
	}
	name, err := scanString(dbValue)
	if err != nil {
		return fmt.Errorf("protoenum serializer: field %s: %w", field.Name, err)
	}
	baseType := field.FieldType
	isPtr := baseType.Kind() == reflect.Pointer
	if isPtr {
		baseType = baseType.Elem()
	}
	inst := reflect.New(baseType).Elem()
	enum, ok := inst.Interface().(protoreflect.Enum)
	if !ok {
		return fmt.Errorf("protoenum serializer: field %s type %s is not a proto enum", field.Name, baseType)
	}
	descriptor := enum.Descriptor().Values().ByName(protoreflect.Name(name))
	if descriptor == nil {
		return fmt.Errorf("protoenum serializer: field %s has unexpected enum value %q", field.Name, name)
	}
	inst.SetInt(int64(descriptor.Number()))
	if isPtr {
		ptr := reflect.New(baseType)
		ptr.Elem().Set(inst)
		target.Set(ptr)
		return nil
	}
	target.Set(inst)
	return nil
}

// protoJSONSerializer stores a proto message as protojson in a JSONB column and parses it back. The
// field type must be a pointer to a proto message; a nil pointer or NULL column round-trips as SQL
// NULL / a nil pointer.
type protoJSONSerializer struct{}

func (protoJSONSerializer) Value(_ context.Context, field *schema.Field, _ reflect.Value, fieldValue interface{}) (interface{}, error) {
	if fieldValue == nil {
		return nil, nil
	}
	if rv := reflect.ValueOf(fieldValue); rv.Kind() == reflect.Pointer && rv.IsNil() {
		return nil, nil
	}
	message, ok := fieldValue.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("protojson serializer: field %s value %T is not a proto message", field.Name, fieldValue)
	}
	encoded, err := protojson.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("protojson serializer: marshaling field %s: %w", field.Name, err)
	}
	return string(encoded), nil
}

func (protoJSONSerializer) Scan(ctx context.Context, field *schema.Field, dst reflect.Value, dbValue interface{}) error {
	target := field.ReflectValueOf(ctx, dst)
	if dbValue == nil {
		return nil // leave the nil pointer
	}
	if field.FieldType.Kind() != reflect.Pointer {
		return fmt.Errorf("protojson serializer: field %s type %s is not a pointer to a proto message", field.Name, field.FieldType)
	}
	raw, err := scanBytes(dbValue)
	if err != nil {
		return fmt.Errorf("protojson serializer: field %s: %w", field.Name, err)
	}
	newPtr := reflect.New(field.FieldType.Elem())
	message, ok := newPtr.Interface().(proto.Message)
	if !ok {
		return fmt.Errorf("protojson serializer: field %s type %s is not a proto message", field.Name, field.FieldType)
	}
	if len(raw) > 0 {
		if err := protojson.Unmarshal(raw, message); err != nil {
			return fmt.Errorf("protojson serializer: unmarshaling field %s: %w", field.Name, err)
		}
	}
	target.Set(newPtr)
	return nil
}

// scanString coerces a driver value (string or []byte) to a string.
func scanString(dbValue interface{}) (string, error) {
	switch value := dbValue.(type) {
	case string:
		return value, nil
	case []byte:
		return string(value), nil
	default:
		return "", fmt.Errorf("expected string/[]byte, got %T", dbValue)
	}
}

// scanBytes coerces a driver value (string or []byte) to bytes.
func scanBytes(dbValue interface{}) ([]byte, error) {
	switch value := dbValue.(type) {
	case []byte:
		return value, nil
	case string:
		return []byte(value), nil
	default:
		return nil, fmt.Errorf("expected string/[]byte, got %T", dbValue)
	}
}
