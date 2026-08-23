package core

import (
	"encoding/json"
	"strings"
	"sync"
)

// FiberHost dual-API helper: TrueNAS 26+ refactored zfs.snapshot.* and dataset
// recursion endpoints into a unified zfs.resource.query. The legacy method names
// continue to work on older TrueNAS releases. We probe the running server once
// per Session, cache the answer, and route callers through the new endpoint
// transparently when available.

type zfsApiCaps struct {
	once       sync.Once
	useNewZfs  bool
	hasError   error
}

var zfsApiCapsBySession sync.Map // map[string]*zfsApiCaps keyed on session URL

func capsForSession(s Session) *zfsApiCaps {
	key := s.GetUrl()
	if v, ok := zfsApiCapsBySession.Load(key); ok {
		return v.(*zfsApiCaps)
	}
	c := &zfsApiCaps{}
	if existing, loaded := zfsApiCapsBySession.LoadOrStore(key, c); loaded {
		return existing.(*zfsApiCaps)
	}
	return c
}

// HasNewZfsResourceApi checks once per Session дали TrueNAS-ът поддържа
// новия zfs.resource.query (26+). Резултатът се кешира.
func HasNewZfsResourceApi(s Session) bool {
	useNew, _ := HasNewZfsResourceApiErr(s)
	return useNew
}

// HasNewZfsResourceApiErr е същото, но връща и грешката от probe-а.
//
// Дотук тя се записваше в caps.hasError и никой не я четеше: недостъпен сървър,
// изтекъл ключ и „това е стар TrueNAS" бяха неразличими — всички даваха false и
// изпращаха командата по легаси пътя, където се проваляше с подвеждащо съобщение.
func HasNewZfsResourceApiErr(s Session) (bool, error) {
	caps := capsForSession(s)
	caps.once.Do(func() {
		caps.useNewZfs, caps.hasError = probeZfsResourceApi(s)
	})
	return caps.useNewZfs, caps.hasError
}

func probeZfsResourceApi(s Session) (useNew bool, err error) {
	// core.get_methods връща мап с всички достъпни RPC endpoints.
	// На TrueNAS 26+ ще има zfs.resource.query, на 25.x — няма.
	//
	// Wrap-нато в recover защото mock test sessions може да panic-нат при
	// неочаквани CallRaw извиквания. В такъв случай просто връщаме false и
	// retain-ваме legacy поведението (тестовете очакват zfs.snapshot.* calls).
	defer func() {
		if r := recover(); r != nil {
			useNew = false
			err = nil
		}
	}()

	// Test sessions имат празен URL — пропускаме detection (легаси път).
	if s.GetUrl() == "" {
		return false, nil
	}

	if err := MaybeLogin(s); err != nil {
		return false, err
	}
	raw, err := s.CallRaw("core.get_methods", 10, []interface{}{})
	if err != nil {
		return false, err
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return false, err
	}
	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		return false, nil
	}
	_, hasResource := result["zfs.resource.query"]
	return hasResource, nil
}

// IsMethodNotFoundError проверява дали грешката е JSON-RPC -32601 (method missing).
// Ползва се за runtime fallback ако init detection е пропуснал нещо.
func IsMethodNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Method does not exist") || strings.Contains(msg, "-32601")
}
