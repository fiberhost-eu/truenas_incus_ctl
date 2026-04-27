package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"truenas/truenas_incus_ctl/core"
)

// queryZfsResourceCompat изпълнява заявка през новия zfs.resource.query (TrueNAS 26+)
// и преобразува резултата към legacy типовия формат, който останалата logic ползва.
//
// targetType филтрира resource типа: "SNAPSHOT", "FILESYSTEM" или "VOLUME".
// Празен низ означава всички типове.
//
// Параметри в zfs.resource.query (single object arg):
//
//	{
//	  paths:        []string  // конкретни ZFS paths за query (parent dataset(s))
//	  properties:   []string  // ZFS свойства (available, used, ...). nil = default set
//	  get_children: bool      // вземи и children (необходимо за snapshot listing)
//	  max_depth:    int       // 0 = неограничено, ползваме за recursive queries
//	}
//
// Връща списък от обекти със shape:
//
//	{ name, type, pool, properties: { X: { value, raw, source } }, children: [...] }
//
// Преобразуваме всеки resource в map със flat properties (legacy compatible).
func queryZfsResourceCompat(api core.Session, entries, entryTypes, propsList []string, params typeQueryParams, targetType string) (typeQueryResponse, error) {
	response := typeQueryResponse{}

	paths, snapshotNames := extractPathsForResourceQuery(entries, entryTypes)
	getChildren := params.shouldRecurse || targetType == "SNAPSHOT" || len(snapshotNames) > 0

	properties := buildResourceProperties(propsList, params, targetType == "SNAPSHOT")

	queryArg := map[string]interface{}{
		"paths":         paths,
		"properties":    properties,
		"get_children":  getChildren,
		"nest_results":  false,
		"max_depth":     0,
	}

	DebugJson([]interface{}{queryArg})

	data, err := core.ApiCall(api, "zfs.resource.query", defaultCallTimeout, []interface{}{queryArg})
	if err != nil {
		return response, err
	}

	var jsonResp map[string]interface{}
	if err = json.Unmarshal(data, &jsonResp); err != nil {
		return response, fmt.Errorf("response error: %v", err)
	}

	resultsList, errMsg := core.ExtractJsonArrayOfMaps(jsonResp, "result")
	if errMsg != "" {
		return response, errors.New("API response results: " + errMsg)
	}
	if len(resultsList) == 0 {
		DebugString("zfs.resource.query returned empty result")
		return response, nil
	}

	outputMap := make(map[string]map[string]interface{})
	intKeys := make([]int, 0)
	strKeys := make([]string, 0)

	// Recursive flatten — children идват вложени в parent.
	flat := flattenResourceTree(resultsList)

	wantSnapshotSet := buildLookupSet(snapshotNames)

	for _, item := range flat {
		itemType, _ := item["type"].(string)
		if targetType != "" && !strings.EqualFold(itemType, targetType) {
			continue
		}

		name, _ := item["name"].(string)
		if name == "" {
			continue
		}

		// Ако имаме explicit snapshot lookups, филтрираме по тях
		if len(wantSnapshotSet) > 0 && !wantSnapshotSet[name] {
			continue
		}

		dict := convertResourceToLegacyDict(item, params.valueOrder)
		dict["id"] = name

		if _, exists := outputMap[name]; exists {
			continue
		}
		outputMap[name] = dict

		if !params.shouldSkipKeyBuild {
			if n, errNotNumber := strconv.Atoi(name); errNotNumber == nil {
				intKeys = append(intKeys, n)
			} else {
				strKeys = append(strKeys, name)
			}
		}
	}

	response = typeQueryResponse{
		resultsMap: outputMap,
		intKeys:    intKeys,
		strKeys:    strKeys,
	}
	return response, nil
}

// extractPathsForResourceQuery връща уникалните parent dataset paths за zfs.resource.query
// и списък с конкретно искани snapshot имена (за post-filter).
func extractPathsForResourceQuery(entries, entryTypes []string) ([]string, []string) {
	pathSet := make(map[string]bool)
	snapshotNames := make([]string, 0)

	for i, entry := range entries {
		if entry == "" {
			continue
		}
		t := ""
		if i < len(entryTypes) {
			t = entryTypes[i]
		}

		switch t {
		case "snapshot", "name":
			// snapshot форма: "dataset@name". Parent = dataset.
			if at := strings.Index(entry, "@"); at > 0 {
				pathSet[entry[:at]] = true
				snapshotNames = append(snapshotNames, entry)
			} else {
				// просто dataset path
				pathSet[entry] = true
			}
		case "snapshot_name":
			// само името след @ — не можем да определим parent. Използваме празно
			// (което в zfs.resource.query означава "всички").
			snapshotNames = append(snapshotNames, entry)
		case "dataset", "pool", "":
			pathSet[entry] = true
		default:
			pathSet[entry] = true
		}
	}

	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	return paths, snapshotNames
}

// buildResourceProperties преобразува propsList и flags към списъка properties за query.
// Връща nil за "default set" (което приема API-то).
func buildResourceProperties(propsList []string, params typeQueryParams, isSnapshot bool) []string {
	if params.shouldGetAllProps {
		return nil // default = all
	}
	if len(propsList) == 0 {
		// За snapshot: основни props за legacy parity
		if isSnapshot {
			return []string{"name", "createtxg"}
		}
		return []string{"available", "used", "type"}
	}

	out := make([]string, 0, len(propsList)+2)
	hasType := false
	hasCreatetxg := false
	for _, p := range propsList {
		out = append(out, p)
		if p == "type" {
			hasType = true
		}
		if p == "createtxg" {
			hasCreatetxg = true
		}
	}
	if !hasType {
		out = append(out, "type")
	}
	if isSnapshot && !hasCreatetxg {
		out = append(out, "createtxg")
	}
	return out
}

// flattenResourceTree обхожда nested children и връща flat списък.
func flattenResourceTree(items []map[string]interface{}) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(items))
	stack := append([]map[string]interface{}{}, items...)
	for len(stack) > 0 {
		item := stack[0]
		stack = stack[1:]
		result = append(result, item)

		children, _ := core.ExtractJsonArrayOfMaps(item, "children")
		if len(children) > 0 {
			stack = append(stack, children...)
		}
	}
	return result
}

// convertResourceToLegacyDict разопакова properties от {value, raw, source} → flat.
// Запазва createtxg и type на root level (ако ги има).
func convertResourceToLegacyDict(item map[string]interface{}, valueOrder []string) map[string]interface{} {
	dict := make(map[string]interface{})

	// Top-level полета
	for _, k := range []string{"name", "type", "pool", "createtxg", "guid"} {
		if v, ok := item[k]; ok {
			dict[k] = v
		}
	}

	// properties = {prop: {raw, value, source}}
	if props, ok := item["properties"].(map[string]interface{}); ok {
		for propName, propValue := range props {
			propMap, ok := propValue.(map[string]interface{})
			if !ok {
				dict[propName] = propValue
				continue
			}

			// valueOrder диктува коя стойност предпочитаме (parsed/raw/value).
			// Default: value > raw > source.
			placed := false
			for _, key := range valueOrder {
				if val, exists := propMap[key]; exists && val != nil {
					dict[propName] = val
					placed = true
					break
				}
			}
			if !placed {
				if val, exists := propMap["value"]; exists && val != nil {
					dict[propName] = val
				} else if raw, exists := propMap["raw"]; exists && raw != nil {
					dict[propName] = raw
				}
			}
		}
	}

	// user_properties (rarely set, но за completeness)
	if userProps, ok := item["user_properties"].(map[string]interface{}); ok {
		for k, v := range userProps {
			if _, exists := dict[k]; !exists {
				dict[k] = v
			}
		}
	}

	return dict
}

func buildLookupSet(items []string) map[string]bool {
	if len(items) == 0 {
		return nil
	}
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[item] = true
	}
	return set
}
