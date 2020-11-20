// Copyright (c) 2020 KHS Films
//
// This file is a part of mtproto package.
// See https://github.com/xelaj/mtproto/blob/master/LICENSE for details

package tl

import (
	"bytes"
	"fmt"
	"reflect"

	"github.com/pkg/errors"
)

func Decode(data []byte, v any) error {
	if v == nil {
		return errors.New("can't unmarshal to nil value")
	}
	if reflect.TypeOf(v).Kind() != reflect.Ptr {
		return fmt.Errorf("v value is not pointer as expected. got %v", reflect.TypeOf(v))
	}

	d := NewDecoder(bytes.NewReader(data))

	d.decodeValue(reflect.ValueOf(v))
	if d.err != nil {
		return errors.Wrapf(d.err, "decode %T", v)
	}

	return nil
}

func (d *Decoder) decodeObject(o Object, ignoreCRC bool) {
	if d.err != nil {
		return
	}

	if !ignoreCRC {
		crcCode := d.PopCRC()
		if d.err != nil {
			d.err = errors.Wrap(d.err, "read crc")
			return
		}

		if crcCode != o.CRC() {
			d.err = fmt.Errorf("invalid crc code: %#v, want: %#v", crcCode, o.CRC())
			return
		}
	}

	value := reflect.ValueOf(o)
	if value.Kind() != reflect.Ptr {
		panic("not a pointer")
	}

	value = reflect.Indirect(value)
	if value.Kind() != reflect.Struct {
		panic("not receiving on struct: " + value.Type().String() + " -> " + value.Kind().String())
	}

	vtyp := value.Type()
	var optionalBitSet uint32
	if haveFlag(value.Interface()) {
		bitset := d.PopUint()
		if d.err != nil {
			d.err = errors.Wrap(d.err, "read bitset")
			return
		}

		optionalBitSet = bitset
	}

	for i := 0; i < value.NumField(); i++ {
		field := value.Field(i)

		if _, found := vtyp.Field(i).Tag.Lookup(tagName); found {
			info, err := parseTag(vtyp.Field(i).Tag)
			if err != nil {
				d.err = errors.Wrap(err, "parse tag")
				return
			}

			if optionalBitSet&(1<<info.index) == 0 {
				continue
			}

			if info.encodedInBitflag {
				field.Set(reflect.ValueOf(true).Convert(field.Type()))
				continue
			}
		}

		if field.Kind() == reflect.Ptr { // && field.IsNil()
			val := reflect.New(field.Type().Elem())
			field.Set(val)
		}

		d.decodeValue(field)
		if d.err != nil {
			d.err = errors.Wrapf(d.err, "decode field '%s'", vtyp.Field(i).Name)
		}
	}
}

func (d *Decoder) decodeValue(value reflect.Value) {
	if d.err != nil {
		return
	}

	if m, ok := value.Interface().(Unmarshaler); ok {
		err := m.UnmarshalTL(d)
		if err != nil {
			d.err = err
			return
		}
	}

	val := d.decodeValueGeneral(value)
	if val != nil {
		value.Set(reflect.ValueOf(val).Convert(value.Type()))
		return
	}

	switch value.Kind() { //nolint:exhaustive has default case + more types checked
	// Float64,Int64,Uint32,Int32,Bool,String,Chan, Func, Uintptr, UnsafePointer,Struct,Map,Array,Int,
	// Int8,Int16,Uint,Uint8,Uint16,Uint64,Float32,Complex64,Complex128
	// these values are checked already

	case reflect.Slice:
		if _, ok := value.Interface().([]byte); ok {
			val = d.PopMessage()
		} else {
			val = d.PopVector(value.Type().Elem())
		}

	case reflect.Ptr:
		if o, ok := value.Interface().(Object); ok {
			d.decodeObject(o, false)
		} else {
			d.decodeValue(value.Elem())
		}

		return

	case reflect.Interface:
		val = d.decodeRegisteredObject()
		if d.err != nil {
			d.err = errors.Wrap(d.err, "decode interface")
			return
		}
	default:
		panic("неизвестная штука: " + value.Type().String())
	}

	if d.err != nil {
		return
	}

	value.Set(reflect.ValueOf(val).Convert(value.Type()))
}

// декодирует базовые типы, строчки числа, вот это. если тип не найден возвращает nil
func (d *Decoder) decodeValueGeneral(value reflect.Value) interface{} {
	// value, which is setting into value arg
	var val interface{}

	switch value.Kind() { //nolint:exhaustive has default case
	case reflect.Float64:
		val = d.PopDouble()

	case reflect.Int64:
		val = d.PopLong()

	case reflect.Uint32: // это применимо так же к енумам
		val = d.PopUint()

	case reflect.Int32:
		val = int32(d.PopUint())

	case reflect.Bool:
		val = d.PopBool()

	case reflect.String:
		val = string(d.PopMessage())

	case reflect.Chan, reflect.Func, reflect.Uintptr, reflect.UnsafePointer:
		panic(value.Kind().String() + " does not supported")

	case reflect.Struct:
		d.err = fmt.Errorf("%v must implement tl.Object for decoding (also it must be pointer)", value.Type())

	case reflect.Map:
		d.err = errors.New("map is not ordered object (must order like structs)")

	case reflect.Array:
		d.err = errors.New("array must be slice")

	case reflect.Int, reflect.Int8, reflect.Int16,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint64:
		d.err = fmt.Errorf("int kind: %v (must converted to int32, int64 or uint32 explicitly)", value.Kind())
		return nil

	case reflect.Float32, reflect.Complex64, reflect.Complex128:
		d.err = fmt.Errorf("float kind: %s (must be converted to float64 explicitly)", value.Kind())
		return nil

	default:
		// not basic type
		return nil
	}

	return val
}

func DecodeRegistered(data []byte) (Object, error) {
	decoder := NewDecoder(bytes.NewReader(data))
	ob := decoder.decodeRegisteredObject()

	if decoder.err != nil {
		return nil, errors.Wrap(decoder.err, "decode registered object")
	}

	return ob, nil
}

func (d *Decoder) decodeRegisteredObject() Object {
	crc := d.PopCRC()
	if d.err != nil {
		d.err = errors.Wrap(d.err, "read crc")
	}

	_typ, ok := objectByCrc[crc]
	if !ok {
		msg, err := d.DumpWithoutRead()
		if err != nil {
			return nil
		}

		d.err = ErrRegisteredObjectNotFound{
			Crc:  crc,
			Data: msg,
		}

		return nil
	}

	o := reflect.New(_typ.Elem()).Interface().(Object)

	if m, ok := o.(Unmarshaler); ok {
		err := m.UnmarshalTL(d)
		if err != nil {
			d.err = err
			return nil
		}
		return o
	}

	if _, isEnum := enumCrcs[crc]; !isEnum {
		d.decodeObject(o, true)
		if d.err != nil {
			d.err = errors.Wrapf(d.err, "decode registered object %T", o)
			return nil
		}
	}

	return o
}