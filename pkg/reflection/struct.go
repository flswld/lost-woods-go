// 结构体反射工具集
//
// 主要用途：动态读写 PB 对象字段（不需要为每个 PB 类型写 setter/getter）
//
// 项目用例（gs/game/game.go）：
//   - SendError: 通过 SetStructFieldValue("Retcode", code) 给任意 Rsp 设置错误码
//     · 不需要 type switch 或 type assertion
//     · 通用 SendError(cmdId, player, rsp, retCode) 适用所有 Rsp 类型
//
// 提供：
//   - ConvStructToMap: 结构体 → map[string]any（字段名 → 值）
//   - GetStructFieldValue: 按字段名取值
//   - SetStructFieldValue: 按字段名设值
//   - CopyStructField: 字段间拷贝（src.field → dst.field）

package reflection

import (
	"reflect"
)

func ConvStructToMap(value any) map[string]any {
	refType := reflect.TypeOf(value)
	if refType.Kind() == reflect.Ptr {
		refType = refType.Elem()
	}
	if refType.Kind() != reflect.Struct {
		return nil
	}
	fieldNum := refType.NumField()
	nameList := make([]string, 0)
	for i := 0; i < fieldNum; i++ {
		nameList = append(nameList, refType.Field(i).Name)
	}
	refValue := reflect.ValueOf(value)
	if refValue.Kind() == reflect.Ptr {
		refValue = refValue.Elem()
	}
	result := make(map[string]any)
	for i := 0; i < fieldNum; i++ {
		result[nameList[i]] = refValue.FieldByName(nameList[i]).Interface()
	}
	return result
}

func GetStructFieldValue(structPointer any, fieldName string) (bool, any) {
	refType := reflect.TypeOf(structPointer)
	if refType.Kind() != reflect.Ptr {
		return false, nil
	}
	refType = refType.Elem()
	if refType.Kind() != reflect.Struct {
		return false, nil
	}
	refValue := reflect.ValueOf(structPointer)
	if refValue.Kind() != reflect.Ptr {
		return false, nil
	}
	refValue = refValue.Elem()
	field := refValue.FieldByName(fieldName)
	return true, field.Interface()
}

func SetStructFieldValue(structPointer any, fieldName string, value any) bool {
	refType := reflect.TypeOf(structPointer)
	if refType.Kind() != reflect.Ptr {
		return false
	}
	refType = refType.Elem()
	if refType.Kind() != reflect.Struct {
		return false
	}
	refValue := reflect.ValueOf(structPointer)
	if refValue.Kind() != reflect.Ptr {
		return false
	}
	refValue = refValue.Elem()
	field := refValue.FieldByName(fieldName)
	if field.Type() != reflect.TypeOf(value) {
		return false
	}
	field.Set(reflect.ValueOf(value))
	return true
}

func CopyStructField(dst any, src any, fieldName string) bool {
	ok, value := GetStructFieldValue(src, fieldName)
	if !ok {
		return false
	}
	ok = SetStructFieldValue(dst, fieldName, value)
	if !ok {
		return false
	}
	return true
}

func CopyStructSameField(dst any, src any) bool {
	// dst
	dstRefType := reflect.TypeOf(dst)
	if dstRefType.Kind() != reflect.Ptr {
		return false
	}
	dstRefType = dstRefType.Elem()
	if dstRefType.Kind() != reflect.Struct {
		return false
	}
	dstRefValue := reflect.ValueOf(dst)
	if dstRefValue.Kind() != reflect.Ptr {
		return false
	}
	dstRefValue = dstRefValue.Elem()
	// src
	srcRefType := reflect.TypeOf(src)
	if srcRefType.Kind() != reflect.Ptr {
		return false
	}
	srcRefType = srcRefType.Elem()
	if srcRefType.Kind() != reflect.Struct {
		return false
	}
	srcRefValue := reflect.ValueOf(src)
	if srcRefValue.Kind() != reflect.Ptr {
		return false
	}
	srcRefValue = srcRefValue.Elem()
	// copy
	fieldNum := srcRefType.NumField()
	for i := 0; i < fieldNum; i++ {
		srcFieldType := srcRefType.Field(i)
		if !srcFieldType.IsExported() {
			continue
		}
		fieldName := srcFieldType.Name
		dstFieldType, ok := dstRefType.FieldByName(fieldName)
		if !ok {
			continue
		}
		srcField := srcRefValue.FieldByName(fieldName)
		dstField := dstRefValue.FieldByName(fieldName)
		if srcField.Kind() == reflect.Ptr {
			dstField.Set(reflect.New(dstFieldType.Type.Elem()))
			CopyStructSameField(dstField.Interface(), srcField.Interface())
			continue
		}
		if dstField.Type() != reflect.TypeOf(srcField.Interface()) {
			return false
		}
		dstField.Set(reflect.ValueOf(srcField.Interface()))
	}
	return true
}
