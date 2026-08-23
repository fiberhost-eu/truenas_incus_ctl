package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"truenas/truenas_incus_ctl/core"
)

// TrueNAS 26 премахна целия namespace zfs.snapshot.* (проверено на 26.0.0-BETA.3:
// 815 метода, нито един от тях). Заместникът е zfs.resource.snapshot.query — друг
// метод с друга сигнатура, не преименуване.
//
// Различия, всяко от които е капан:
//
//   - `recursive` вместо get_children/max_depth/nest_results; резултатът е ПЛОСЪК,
//     без вложени children.
//   - `properties` НЯМА „по подразбиране" и НЯМА „всички": липсващо, [] и null дават
//     нула свойства. Затова за „всички" подаваме изричен списък (snapshotAllProperties).
//   - Непознато име на свойство се изхвърля МЪЛЧАЛИВО, без грешка. Точно това чупи
//     `clones`: заявка за него връща отговор без ключа, а не отказ.
//   - Числата в JSON минават през float64 и губят точност: guid идва като
//     11590773493371666000, докато raw е "11590773493371667270". Затова стойностите
//     се четат от `raw`, не от `value`.
var snapshotAllProperties = []string{
	"used", "referenced", "written", "logicalreferenced", "creation", "createtxg",
	"guid", "type", "compressratio", "refcompressratio", "objsetid", "volsize", "encryption",
}

// Свойства, които zfs.resource.snapshot.query отказва. `clones` се набавя с обратна
// справка по origin; останалите просто ги няма за снапшот.
var snapshotUnsupportedProperties = map[string]bool{
	"clones": true, "origin": true, "name": true, "defer_destroy": true,
	"userrefs": true, "unique": true, "mountpoint": true,
	"usedbysnapshots": true, "available": true, "id": true,
}

// querySnapshotResourceCompat заменя zfs.snapshot.query за TrueNAS 26+.
func querySnapshotResourceCompat(api core.Session, entries, entryTypes, propsList []string, params typeQueryParams) (typeQueryResponse, error) {
	response := typeQueryResponse{}

	paths, wantedSnapshots := extractSnapshotQueryPaths(entries, entryTypes)

	properties, wantsClones := buildSnapshotProperties(propsList, params)

	queryArg := map[string]interface{}{
		"paths":      paths,
		"properties": properties,
		// Изричното име на снапшот е точен адрес; всичко друго е датасет, от който
		// искаме снапшотите му.
		"recursive":           params.shouldRecurse,
		"get_user_properties": params.shouldGetUserProps,
	}

	DebugJson([]interface{}{queryArg})

	data, err := core.ApiCall(api, "zfs.resource.snapshot.query", defaultCallTimeout, []interface{}{queryArg})
	if err != nil {
		// Несъществуващ път вече е ГРЕШКА, а не празен резултат: старото
		// zfs.snapshot.query работеше с филтри и просто не връщаше нищо.
		// Incus разчита на това — при `list -t filesystem,volume,snapshot` върху
		// том, който тепърва се създава, заявката за снапшоти трябва да е празна,
		// иначе вали цялото изброяване.
		//
		// Прехваща се САМО „няма такъв път". Липсващ метод (-32601) си остава
		// грешка — заглушаването му е причината Incus да чуе „няма клонинги".
		if isPathNotFoundError(err) {
			DebugString("zfs.resource.snapshot.query: пътят не съществува, празен резултат")
			return typeQueryResponse{
				resultsMap: make(map[string]map[string]interface{}),
				intKeys:    make([]int, 0),
				strKeys:    make([]string, 0),
			}, nil
		}
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

	wantSet := buildLookupSet(wantedSnapshots)

	// Обратната справка е една заявка за всички засегнати пулове, не по снапшот.
	var clonesBySnapshot map[string][]string
	if wantsClones && len(resultsList) > 0 {
		clonesBySnapshot, err = lookupClonesByOrigin(api, resultsList)
		if err != nil {
			return response, err
		}
	}

	outputMap := make(map[string]map[string]interface{})
	intKeys := make([]int, 0)
	strKeys := make([]string, 0)

	for _, item := range resultsList {
		name, _ := item["name"].(string)
		if name == "" {
			continue
		}
		if wantSet != nil && !wantSet[name] {
			continue
		}
		if _, exists := outputMap[name]; exists {
			continue
		}

		dict := make(map[string]interface{})
		dict["id"] = name

		// Върхните полета (name, dataset, pool, snapshot_name, type…) са прости
		// стойности, не {raw,value,source}.
		insertProperties(dict, item, []string{"id", "properties", "user_properties", "holds", "guid", "createtxg"}, params.valueOrder)

		// guid и createtxg се четат от properties, защото върхните им копия минават
		// през float64 и губят точност при големи стойности.
		if props, ok := item["properties"].(map[string]interface{}); ok {
			insertProperties(dict, normalizeSnapshotProperties(props), nil, params.valueOrder)
		}
		for _, k := range []string{"guid", "createtxg"} {
			if _, exists := dict[k]; !exists {
				if v, ok := item[k]; ok {
					dict[k] = v
				}
			}
		}

		if userProps, ok := item["user_properties"].(map[string]interface{}); ok {
			insertProperties(dict, userProps, nil, params.valueOrder)
		}

		if wantsClones {
			// Празният низ е валиден отговор „няма клонинги" — за разлика от липсващ
			// ключ, който би минал за „не знам".
			dict["clones"] = strings.Join(clonesBySnapshot[name], ",")
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

// normalizeSnapshotProperties привежда {raw, value, source} към формата, който
// insertProperties и valueOrder очакват ({parsed, value, rawvalue}).
//
// Всички числа се вземат от `raw`: JSON стойността е float64 и над 2^53 лъже.
func normalizeSnapshotProperties(props map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(props))

	for name, entry := range props {
		entryMap, ok := entry.(map[string]interface{})
		if !ok {
			out[name] = entry
			continue
		}

		raw, hasRaw := entryMap["raw"]
		value := entryMap["value"]

		display := value
		if _, isString := value.(string); !isString && hasRaw {
			display = raw
		}

		converted := map[string]interface{}{}
		if hasRaw {
			converted["rawvalue"] = raw
			converted["parsed"] = raw
		} else {
			converted["parsed"] = value
		}
		converted["value"] = display

		out[name] = converted
	}

	return out
}

// extractSnapshotQueryPaths връща пътищата за заявката и (когато са поискани
// поименно) точните снапшоти за post-филтър.
//
// zfs.resource.snapshot.query приема както датасет, така и пълен `ds@snap`.
func extractSnapshotQueryPaths(entries, entryTypes []string) ([]string, []string) {
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
			if strings.Contains(entry, "@") {
				pathSet[entry] = true
				snapshotNames = append(snapshotNames, entry)
			} else {
				pathSet[entry] = true
			}
		case "snapshot_name":
			// Само частта след @ — няма как да се посочи родител, филтрира се после.
			snapshotNames = append(snapshotNames, entry)
		default:
			pathSet[entry] = true
		}
	}

	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	return paths, snapshotNames
}

// buildSnapshotProperties маха имената, които новото API отказва, и казва дали
// някой е поискал `clones`.
func buildSnapshotProperties(propsList []string, params typeQueryParams) ([]string, bool) {
	if params.shouldGetAllProps {
		return snapshotAllProperties, true
	}

	out := make([]string, 0, len(propsList)+1)
	seen := make(map[string]bool)
	wantsClones := false

	for _, p := range propsList {
		if p == "clones" {
			wantsClones = true
			continue
		}
		if snapshotUnsupportedProperties[p] || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}

	if !seen["type"] {
		out = append(out, "type")
	}
	if !seen["createtxg"] {
		out = append(out, "createtxg")
	}

	return out, wantsClones
}

// lookupClonesByOrigin набавя `clones` наобратно: свойството не се дава на снапшот,
// но всеки клонинг сочи назад през `origin`.
func lookupClonesByOrigin(api core.Session, snapshots []map[string]interface{}) (map[string][]string, error) {
	poolSet := make(map[string]bool)
	for _, s := range snapshots {
		if pool, _ := s["pool"].(string); pool != "" {
			poolSet[pool] = true
			continue
		}
		if name, _ := s["name"].(string); name != "" {
			if slash := strings.Index(name, "/"); slash > 0 {
				poolSet[name[:slash]] = true
			}
		}
	}

	pools := make([]string, 0, len(poolSet))
	for p := range poolSet {
		pools = append(pools, p)
	}
	sort.Strings(pools)

	if len(pools) == 0 {
		return map[string][]string{}, nil
	}

	queryArg := map[string]interface{}{
		"paths":        pools,
		"properties":   []string{"origin"},
		"get_children": true,
		"nest_results": false,
		"max_depth":    0,
	}

	data, err := core.ApiCall(api, "zfs.resource.query", defaultCallTimeout, []interface{}{queryArg})
	if err != nil {
		return nil, fmt.Errorf("clones lookup failed: %v", err)
	}

	var jsonResp map[string]interface{}
	if err = json.Unmarshal(data, &jsonResp); err != nil {
		return nil, fmt.Errorf("clones lookup response error: %v", err)
	}

	resultsList, errMsg := core.ExtractJsonArrayOfMaps(jsonResp, "result")
	if errMsg != "" {
		return nil, errors.New("clones lookup results: " + errMsg)
	}

	out := make(map[string][]string)
	for _, item := range flattenResourceTree(resultsList) {
		name, _ := item["name"].(string)
		if name == "" {
			continue
		}
		origin := extractOriginValue(item)
		if origin == "" {
			continue
		}
		out[origin] = append(out[origin], name)
	}

	for k := range out {
		sort.Strings(out[k])
	}

	return out, nil
}

// extractOriginValue вади origin-а на един ресурс. Липсата на произход се пише
// „none" (не „-", както в изхода на самия zfs).
func extractOriginValue(item map[string]interface{}) string {
	props, ok := item["properties"].(map[string]interface{})
	if !ok {
		return ""
	}
	entry, ok := props["origin"].(map[string]interface{})
	if !ok {
		return ""
	}

	value := ""
	if raw, isStr := entry["raw"].(string); isStr {
		value = raw
	} else if v, isStr := entry["value"].(string); isStr {
		value = v
	}

	value = strings.TrimSpace(value)
	if value == "" || value == "none" || value == "-" {
		return ""
	}

	return value
}

// isPathNotFoundError разпознава „този датасет/снапшот го няма" от новото API.
//
// TrueNAS 26 връща -32602 (Invalid params) с ENOENT в текста; старото поведение при
// същия вход беше празен списък. Умишлено НЕ покрива -32601 (липсващ метод).
func isPathNotFoundError(err error) bool {
	if err == nil || core.IsMethodNotFoundError(err) {
		return false
	}

	msg := err.Error()

	return strings.Contains(msg, "ENOENT") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "does not exist")
}
