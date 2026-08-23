package cmd

import (
	"truenas/truenas_incus_ctl/core"
)

// Тук живееше queryZfsResourceCompat — маршрутизираше заявките за снапшоти към
// zfs.resource.query и после филтрираше по type == "SNAPSHOT".
//
// Тя не е можела да върне нищо: zfs.resource.query покрива само FILESYSTEM и
// VOLUME, а подаден път със `@` дава -32602. Официалната документация препраща
// изрично към zfs.resource.snapshot.query.
//
// По-лошото е, че мълчеше — празен списък вместо грешка. `snapshot list -o clones`
// излизаше с код 0 и празен изход, тоест Incus питаше „има ли клонинги" преди да
// трие и чуваше „не".
//
// Заместникът е querySnapshotResourceCompat (cmd/zfs_snapshot_compat.go).
// Тези две помощни функции останаха, защото се ползват от него.

// flattenResourceTree обхожда вложените children и връща плосък списък.
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
