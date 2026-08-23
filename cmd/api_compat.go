package cmd

import (
	"encoding/json"
	"truenas/truenas_incus_ctl/core"
)

// FiberHost dual-API compat layer.
//
// TrueNAS 26+ премахна редица legacy RPC методи и въведе нови с често различни
// signatures. Тук интерсептираме API calls, проверяваме capability на сервера и
// route-ваме към новия endpoint където има еквивалент. За стари TrueNAS (<26)
// HasNewZfsResourceApi връща false и path-ът остава непроменен.
//
// Mapping table (виж probe в repo issue tracker):
//
//	zfs.snapshot.create    →  zfs.resource.snapshot.create   (`properties` → `user_properties`)
//	zfs.snapshot.delete    →  zfs.resource.snapshot.destroy  (`[path, opts]` → `{path, ...opts}`)
//	zfs.snapshot.rollback  →  zfs.resource.snapshot.rollback (merge на opts)
//	zfs.snapshot.rename    →  zfs.resource.snapshot.rename   (`[src, dst]` → `{current_name, new_name}`)
//	zfs.snapshot.clone     →  zfs.resource.snapshot.clone    (`dataset_dst` → `dataset`)
//	zfs.snapshot.hold      →  zfs.resource.snapshot.hold     (merge `path` + opts)
//	zfs.snapshot.release   →  zfs.resource.snapshot.release  (merge `path` + opts)
//	service.start          →  service.control                ([action="START", name, opts])
//	service.stop           →  service.control                ([action="STOP",  name, opts])
//	service.restart        →  service.control                ([action="RESTART", name, opts])
//	zfs.dataset.rename     →  pool.dataset.rename            (само името, параметрите съвпадат)

func translateForNewApi(method string, params interface{}) (string, interface{}, bool) {
	switch method {
	case "zfs.snapshot.create":
		return "zfs.resource.snapshot.create", renameKeyInFirstObject(params, "properties", "user_properties"), true
	case "zfs.snapshot.delete":
		return "zfs.resource.snapshot.destroy", mergePathAndOpts(params), true
	case "zfs.snapshot.rollback":
		return "zfs.resource.snapshot.rollback", mergePathAndOpts(params), true
	case "zfs.snapshot.rename":
		return "zfs.resource.snapshot.rename", twoStringsToRenameObj(params), true
	case "zfs.snapshot.clone":
		return "zfs.resource.snapshot.clone", renameKeyInFirstObject(params, "dataset_dst", "dataset"), true
	case "zfs.snapshot.hold":
		return "zfs.resource.snapshot.hold", mergePathAndOpts(params), true
	case "zfs.snapshot.release":
		return "zfs.resource.snapshot.release", mergePathAndOpts(params), true
	case "service.start":
		return "service.control", buildServiceControlParams("START", params), true
	case "service.stop":
		return "service.control", buildServiceControlParams("STOP", params), true
	case "service.restart":
		return "service.control", buildServiceControlParams("RESTART", params), true
	case "zfs.dataset.rename":
		// Единствената разлика е името: и двете приемат [id, {new_name, …}].
		return "pool.dataset.rename", params, true
	}
	return method, params, false
}

// CompatApiCall wrap-ва core.ApiCall с автоматично translation за TrueNAS 26+.
func CompatApiCall(api core.Session, method string, timeout int64, params interface{}) (json.RawMessage, error) {
	if core.HasNewZfsResourceApi(api) {
		if newMethod, newParams, ok := translateForNewApi(method, params); ok {
			return core.ApiCall(api, newMethod, timeout, newParams)
		}
	}
	out, err := core.ApiCall(api, method, timeout, params)
	if err != nil && core.IsMethodNotFoundError(err) {
		// Runtime fallback ако detection е пропуснало.
		if newMethod, newParams, ok := translateForNewApi(method, params); ok {
			return core.ApiCall(api, newMethod, timeout, newParams)
		}
	}
	return out, err
}

// CompatBulkApiCall е wrapper за MaybeBulkApiCall с translation за TrueNAS 26+.
// Запазва оригиналната сигнатура (json.RawMessage, int64 jobId, error).
func CompatBulkApiCall(api core.Session, method string, timeout int64, params []interface{}, objRemap map[string][]interface{}, ignoreErrors bool) (json.RawMessage, int64, error) {
	if core.HasNewZfsResourceApi(api) {
		if newMethod, newParamsRaw, ok := translateForNewApi(method, params); ok {
			newParams, _ := newParamsRaw.([]interface{})
			if newParams == nil {
				newParams = params
			}
			newRemap := remapBulkObjMap(method, objRemap)
			return MaybeBulkApiCall(api, newMethod, timeout, newParams, newRemap, ignoreErrors)
		}
	}
	out, jobId, err := MaybeBulkApiCall(api, method, timeout, params, objRemap, ignoreErrors)
	if err != nil && core.IsMethodNotFoundError(err) {
		if newMethod, newParamsRaw, ok := translateForNewApi(method, params); ok {
			newParams, _ := newParamsRaw.([]interface{})
			if newParams == nil {
				newParams = params
			}
			newRemap := remapBulkObjMap(method, objRemap)
			return MaybeBulkApiCall(api, newMethod, timeout, newParams, newRemap, ignoreErrors)
		}
	}
	return out, jobId, err
}

// CompatBulkApiCallArray е wrapper за MaybeBulkApiCallArray с translation.
// (Използва се за service.start/stop/restart bulk operations.)
func CompatBulkApiCallArray(api core.Session, method string, timeout int64, paramsArray []interface{}, ignoreErrors bool) (json.RawMessage, int64, error) {
	if core.HasNewZfsResourceApi(api) {
		switch method {
		case "service.start", "service.stop", "service.restart":
			action := "START"
			if method == "service.stop" {
				action = "STOP"
			} else if method == "service.restart" {
				action = "RESTART"
			}
			newArr := make([]interface{}, len(paramsArray))
			for i, entry := range paramsArray {
				if arr, ok := entry.([]interface{}); ok && len(arr) >= 2 {
					serviceName := arr[0]
					opts := arr[1]
					newArr[i] = []interface{}{action, serviceName, opts}
				} else if arr, ok := entry.([]interface{}); ok && len(arr) == 1 {
					newArr[i] = []interface{}{action, arr[0], map[string]interface{}{}}
				} else {
					newArr[i] = entry
				}
			}
			return MaybeBulkApiCallArray(api, "service.control", timeout, newArr, ignoreErrors)
		}
	}
	out, jobId, err := MaybeBulkApiCallArray(api, method, timeout, paramsArray, ignoreErrors)
	if err != nil && core.IsMethodNotFoundError(err) {
		if method == "service.start" || method == "service.stop" || method == "service.restart" {
			// Force translate path
			action := "START"
			if method == "service.stop" {
				action = "STOP"
			} else if method == "service.restart" {
				action = "RESTART"
			}
			newArr := make([]interface{}, len(paramsArray))
			for i, entry := range paramsArray {
				if arr, ok := entry.([]interface{}); ok && len(arr) >= 2 {
					newArr[i] = []interface{}{action, arr[0], arr[1]}
				} else {
					newArr[i] = entry
				}
			}
			return MaybeBulkApiCallArray(api, "service.control", timeout, newArr, ignoreErrors)
		}
	}
	return out, jobId, err
}

// renameKeyInFirstObject взима params под формата []interface{}{ {map}, ... } и
// преименува key в първия map. Ако формата е различна — връща params непроменен.
func renameKeyInFirstObject(params interface{}, oldKey, newKey string) interface{} {
	arr, ok := params.([]interface{})
	if !ok || len(arr) == 0 {
		return params
	}
	first, ok := arr[0].(map[string]interface{})
	if !ok {
		return params
	}
	if val, exists := first[oldKey]; exists {
		first[newKey] = val
		delete(first, oldKey)
	}
	return params
}

// mergePathAndOpts превръща [path_string, opts_obj] в [{path: path_string, ...opts}].
// Старите zfs.snapshot.delete/rollback/hold/release ползват тази форма; новите искат
// единен обект.
func mergePathAndOpts(params interface{}) interface{} {
	arr, ok := params.([]interface{})
	if !ok || len(arr) == 0 {
		return params
	}

	var out map[string]interface{}
	pathVal, isPath := arr[0].(string)

	if isPath {
		out = make(map[string]interface{})
		out["path"] = pathVal
		if len(arr) > 1 {
			if opts, ok := arr[1].(map[string]interface{}); ok {
				for k, v := range opts {
					if k == "path" {
						continue
					}
					out[k] = v
				}
			}
		}
	} else if firstObj, ok := arr[0].(map[string]interface{}); ok {
		out = firstObj // вече е object form
	} else {
		return params
	}

	return []interface{}{out}
}

// twoStringsToRenameObj превръща [source, dest] (2 strings) → [{current_name, new_name}].
func twoStringsToRenameObj(params interface{}) interface{} {
	arr, ok := params.([]interface{})
	if !ok || len(arr) < 2 {
		return params
	}
	src, srcOk := arr[0].(string)
	dst, dstOk := arr[1].(string)
	if !srcOk || !dstOk {
		return params
	}
	return []interface{}{map[string]interface{}{
		"current_name": src,
		"new_name":     dst,
	}}
}

// buildServiceControlParams превръща [name, opts] (старо service.start/stop/restart)
// в [action, name, opts] (нов service.control).
func buildServiceControlParams(action string, params interface{}) interface{} {
	arr, ok := params.([]interface{})
	if !ok {
		return params
	}
	if len(arr) == 0 {
		return params
	}
	name := arr[0]
	var opts interface{} = map[string]interface{}{}
	if len(arr) > 1 {
		opts = arr[1]
	}
	return []interface{}{action, name, opts}
}

// remapBulkObjMap пренастройва obj remap keys за новия endpoint.
// Например zfs.snapshot.delete bulk ползва "" key за path entries —
// при destroy тези стойности влизат като "path" вътре в обекта.
func remapBulkObjMap(oldMethod string, m map[string][]interface{}) map[string][]interface{} {
	if m == nil {
		return nil
	}
	out := make(map[string][]interface{}, len(m))
	for k, v := range m {
		newKey := k
		switch oldMethod {
		case "zfs.snapshot.delete", "zfs.snapshot.rollback", "zfs.snapshot.hold", "zfs.snapshot.release":
			if k == "" {
				newKey = "path"
			}
		case "zfs.snapshot.clone":
			if k == "dataset_dst" {
				newKey = "dataset"
			}
		case "zfs.snapshot.create":
			if k == "properties" {
				newKey = "user_properties"
			}
		}
		out[newKey] = v
	}
	return out
}
