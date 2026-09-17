package cmd

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"truenas/truenas_incus_ctl/core"
)

// Помощни функции за NVMe-oF (nvmet.* API на TrueNAS 26+).
//
// Огледало на iscsi_util.go, но структурата отдолу е различна и разликата е важна:
// iSCSI носи portal и initiator ВГРАДЕНИ в target обекта (`groups: [{portal, initiator}]`),
// докато nvmet има две отделни join таблици — `port_subsys` и `host_subsys`. Затова тук
// има EnsureNvmeLink, каквото при iSCSI няма нужда да съществува.
//
// Проверено срещу жив TrueNAS 26.0.0-BETA.3 (815 метода, nvmet.* = 37).

const DEFAULT_NVME_PORT = 4420

// Максималната дължина на NQN по спецификация. IQN е 64 — оттам идва хеширането в
// iscsi_util.go. При 223 знака то на практика не потрябва: най-дългият датасет път у нас
// е далеч под тази граница.
const MAX_NQN_LENGTH = 223

// MakeNvmeSubNqn сглобява subsystem NQN от път на том.
//
// Схемата следва вече заварената по нодовете (`nqn.2025-06.bg.fiberhost:<нещо>`), за да
// не съжителстват два формата. Разделителите се нормализират както при iSCSI: пътят става
// една дума, защото NQN не бива да носи наклонени черти.
func MakeNvmeSubNqn(prefix, vol string) string {
	name := strings.ToLower(vol)
	name = strings.NewReplacer("/", "-", "@", "-", ":", "-", "_", "-", ".", "-").Replace(name)
	name = strings.Trim(name, "-")

	base := "nqn.2025-06.bg.fiberhost:"
	if prefix != "" {
		base += prefix + "-"
	}

	full := base + name
	if len(full) > MAX_NQN_LENGTH {
		room := MAX_NQN_LENGTH - len(base)
		full = base + core.MakeHashedString(vol, room)
	}
	return full
}

// lookupNvmeIdByFilter връща id-то на първия запис, отговарящ на филтъра, или -1.
func lookupNvmeIdByFilter(api core.Session, endpoint string, filter []interface{}) (int, error) {
	params := []interface{}{filter, make(map[string]interface{})}
	out, err := core.ApiCall(api, endpoint+".query", defaultCallTimeout, params)
	if err != nil {
		return -1, err
	}
	var response map[string]interface{}
	if err = json.Unmarshal(out, &response); err != nil {
		return -1, err
	}
	results, _ := response["result"].([]interface{})
	for i := 0; i < len(results); i++ {
		idObj := core.GetIdFromObject(results[i])
		if n, errNotNumber := strconv.Atoi(fmt.Sprint(idObj)); errNotNumber == nil {
			return n, nil
		}
	}
	return -1, nil
}

// queryNvmeIdsByFilter връща id-тата на ВСИЧКИ записи, отговарящи на филтъра.
//
// Филтърът се подава на сървъра, не се филтрира отсам: iscsi.go:336 тегли цялата
// targetextent таблица при всяко създаване и това не мащабира.
func queryNvmeIdsByFilter(api core.Session, endpoint string, filter []interface{}) ([]interface{}, error) {
	params := []interface{}{filter, make(map[string]interface{})}
	out, err := core.ApiCall(api, endpoint+".query", defaultCallTimeout, params)
	if err != nil {
		return nil, err
	}
	var response map[string]interface{}
	if err = json.Unmarshal(out, &response); err != nil {
		return nil, err
	}
	results, _ := response["result"].([]interface{})

	ids := make([]interface{}, 0, len(results))
	for i := 0; i < len(results); i++ {
		ids = append(ids, core.GetIdFromObject(results[i]))
	}
	return ids, nil
}

// createNvmeObject създава запис и връща id-то му.
func createNvmeObject(api core.Session, endpoint string, obj map[string]interface{}) (int, error) {
	out, err := core.ApiCall(api, endpoint+".create", defaultCallTimeout, []interface{}{obj})
	if err != nil {
		return -1, err
	}
	results, _ := core.GetResultsAndErrorsFromApiResponseRaw(out)
	if len(results) > 0 {
		idObj := core.GetIdFromObject(results[0])
		if n, errNotNumber := strconv.Atoi(fmt.Sprint(idObj)); errNotNumber == nil {
			return n, nil
		}
	}
	return -1, nil
}

// EnsureNvmeLink създава ред в join таблица, ако още го няма. Връща id-то и дали е нов.
//
// Филтърът се прави по ВСИЧКИ полета на връзката, а не по нищо — iscsi.go:336 дърпа
// цялата targetextent таблица при всяко create и това не мащабира.
func EnsureNvmeLink(api core.Session, endpoint string, fields map[string]interface{}) (int, bool, error) {
	filter := make([]interface{}, 0, len(fields))
	for k, v := range fields {
		filter = append(filter, []interface{}{k, "=", v})
	}

	id, err := lookupNvmeIdByFilter(api, endpoint, filter)
	if err != nil {
		return -1, false, err
	}
	if id != -1 {
		return id, false, nil
	}

	id, err = createNvmeObject(api, endpoint, fields)
	if err != nil {
		return -1, false, err
	}
	return id, true, nil
}

// LookupNvmeHostOrCreate намира или създава host по hostnqn.
//
// Празен hostnqn значи „който и да е" — тогава subsystem-ът се пуска с allow_any_host и
// изобщо не се прави host запис.
func LookupNvmeHostOrCreate(api core.Session, hostNqn string) (int, error) {
	if hostNqn == "" {
		return -1, nil
	}
	if asInt, errNotNumber := strconv.Atoi(hostNqn); errNotNumber == nil {
		return asInt, nil
	}

	filter := []interface{}{[]interface{}{"hostnqn", "=", hostNqn}}
	id, err := lookupNvmeIdByFilter(api, "nvmet.host", filter)
	if err != nil || id != -1 {
		return id, err
	}

	return createNvmeObject(api, "nvmet.host", map[string]interface{}{"hostnqn": hostNqn})
}

// LookupNvmePort намира порт по id, по адрес, или взима първия включен.
//
// За разлика от iSCSI тук НЕ се създава порт мълчаливо: портът е споделен ресурс на
// целия таргет и случайно създаден втори порт на същия адрес чупи discovery-то.
func LookupNvmePort(api core.Session, spec string) (int, error) {
	if asInt, errNotNumber := strconv.Atoi(spec); errNotNumber == nil {
		return asInt, nil
	}

	filter := make([]interface{}, 0, 2)
	if spec != "" {
		addr := spec
		port := DEFAULT_NVME_PORT
		if idx := strings.LastIndex(spec, ":"); idx >= 0 && !strings.Contains(spec[idx+1:], "]") {
			if p, errNotNumber := strconv.Atoi(spec[idx+1:]); errNotNumber == nil {
				addr = spec[:idx]
				port = p
			}
		}
		addr = stripIpV6Brackets(addr)
		filter = append(filter, []interface{}{"addr_traddr", "=", addr})
		filter = append(filter, []interface{}{"addr_trsvcid", "=", fmt.Sprint(port)})
	}

	id, err := lookupNvmeIdByFilter(api, "nvmet.port", filter)
	if err != nil {
		return -1, err
	}
	if id == -1 {
		if spec == "" {
			return -1, fmt.Errorf("no NVMe-oF port is configured on the server, create one first")
		}
		return -1, fmt.Errorf("no NVMe-oF port matches %q", spec)
	}
	return id, nil
}

// LookupNvmeSubNqn връща subnqn-а на subsystem по име, или "" ако го няма.
//
// NQN-ът НЕ се сглобява отсам: TrueNAS го прави от `basenqn`, който е свойство на уреда.
// Познаване наум работи, докато някой не смени basenqn — и тогава гърми при connect, не
// при създаване, тоест далеч от причината.
func LookupNvmeSubNqn(api core.Session, name string) (string, error) {
	params := []interface{}{
		[]interface{}{[]interface{}{"name", "=", name}},
		make(map[string]interface{}),
	}
	out, err := core.ApiCall(api, "nvmet.subsys.query", defaultCallTimeout, params)
	if err != nil {
		return "", err
	}
	var response map[string]interface{}
	if err = json.Unmarshal(out, &response); err != nil {
		return "", err
	}
	results, _ := response["result"].([]interface{})
	for _, r := range results {
		if row, ok := r.(map[string]interface{}); ok {
			if nqn, ok := row["subnqn"].(string); ok && nqn != "" {
				return nqn, nil
			}
		}
	}
	return "", nil
}

// LookupNvmePortAddress връща адреса и услугата на порт по id.
func LookupNvmePortAddress(api core.Session, portId int) (string, int, error) {
	params := []interface{}{
		[]interface{}{[]interface{}{"id", "=", portId}},
		make(map[string]interface{}),
	}
	out, err := core.ApiCall(api, "nvmet.port.query", defaultCallTimeout, params)
	if err != nil {
		return "", 0, err
	}
	var response map[string]interface{}
	if err = json.Unmarshal(out, &response); err != nil {
		return "", 0, err
	}
	results, _ := response["result"].([]interface{})
	for _, r := range results {
		row, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		addr, _ := row["addr_traddr"].(string)
		port := DEFAULT_NVME_PORT
		if svc, ok := row["addr_trsvcid"].(float64); ok {
			port = int(svc)
		} else if svcStr, ok := row["addr_trsvcid"].(string); ok {
			if n, errNotNumber := strconv.Atoi(svcStr); errNotNumber == nil {
				port = n
			}
		}
		if addr != "" {
			return addr, port, nil
		}
	}
	return "", 0, fmt.Errorf("NVMe-oF port %d has no listen address", portId)
}

// CheckRemoteNvmetServiceIsRunning връща човешко съобщение, ако услугата не върви.
func CheckRemoteNvmetServiceIsRunning(api core.Session) (string, error) {
	out, err := core.ApiCall(api, "service.started", defaultCallTimeout, []interface{}{"nvmet"})
	if err != nil {
		return "", err
	}
	var response map[string]interface{}
	if err = json.Unmarshal(out, &response); err != nil {
		return "", err
	}
	if !core.IsValueTrue(response, "result") {
		return "The NVMe-oF target service has not been started\nRun this tool with:\nservice start --enable nvmet\nTo start the service", nil
	}
	return "", nil
}
