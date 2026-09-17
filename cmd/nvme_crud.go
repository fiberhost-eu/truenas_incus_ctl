package cmd

import (
	"github.com/spf13/cobra"
)

// Автоматично сглобени CRUD команди за nvmet.* — `share nvme <категория> list|create|update|delete`.
//
// Ползват СЪЩИТЕ генерични функции като iSCSI (iscsiCrudList/UpdateCreate/Delete/Query).
// Те бяха параметризирани по API пространство вместо да се копират: 296 реда обща логика,
// в която името на пространството стоеше зашито на пет места.
//
// Стойностите тук са сверени срещу жив TrueNAS 26.0.0-BETA.3 през core.get_methods, не са
// преписани от документация.

var nvmetCrudCategories = []string{"subsys", "namespace", "port", "host", "port_subsys", "host_subsys"}

// Полетата, по които се ТЪРСИ. Не всяко поле, приемано при създаване, става за заявка:
// `nvmet.namespace.query` приема `subsys_id` и връща нула реда, защото отговорът разгръща
// връзката и полето за търсене е `subsys.id`. Затова тук стоят само доказано валидни имена.
var nvmetCrudIdentifierMap = map[string][]string{
	"subsys":      []string{"id", "name", "subnqn"},
	"namespace":   []string{"id", "device_path"},
	"port":        []string{"id", "addr_traddr", "addr_trsvcid"},
	"host":        []string{"id", "hostnqn"},
	"port_subsys": []string{"id"},
	"host_subsys": []string{"id"},
}

var nvmetCrudRequiredAttrMap = map[string][]string{
	"subsys":      []string{"name"},
	"namespace":   []string{"device_type", "device_path", "subsys_id"},
	"port":        []string{"addr_trtype", "addr_traddr"},
	"host":        []string{"hostnqn"},
	"port_subsys": []string{"port_id", "subsys_id"},
	"host_subsys": []string{"host_id", "subsys_id"},
}

var nvmetNamespaceCreateEnums map[string][]string
var nvmetPortCreateEnums map[string][]string

var nvmetCrudFeatureMap = map[string]map[string]iscsiCrudFeature{
	"subsys": map[string]iscsiCrudFeature{
		"name": iscsiCrudFeature{kind: "String", defValue: "", description: "Subsystem name"},
		"subnqn": iscsiCrudFeature{kind: "String", defValue: "", description: "" +
			"NQN of the subsystem. Left empty, TrueNAS derives it from the global basenqn"},
		"allow-any-host": iscsiCrudFeature{kind: "Bool", defValue: false, description: "Allow any initiator to connect"},
		"pi-enable":      iscsiCrudFeature{kind: "Bool", defValue: false, description: "Protection information"},
		"qid-max":        iscsiCrudFeature{kind: "Int", defValue: 0, description: "Maximum queue id"},
		"ieee-oui":       iscsiCrudFeature{kind: "String", defValue: "", description: "IEEE OUI"},
		"ana":            iscsiCrudFeature{kind: "Bool", defValue: false, description: "Asymmetric namespace access"},
	},
	"namespace": map[string]iscsiCrudFeature{
		"nsid": iscsiCrudFeature{kind: "Int", defValue: 0, description: "Namespace id, unique within the subsystem. Empty picks the next free one"},
		"device-type": iscsiCrudFeature{kind: "String", defValue: "ZVOL", description: "" +
			AddFlagsEnum(&nvmetNamespaceCreateEnums, "device-type", []string{"ZVOL", "FILE"})},
		"device-path": iscsiCrudFeature{kind: "String", defValue: "", description: "Path to the zvol or file"},
		"filesize":    iscsiCrudFeature{kind: "SizeString", defValue: "", description: "File size, for FILE namespaces"},
		"enabled":     iscsiCrudFeature{kind: "Bool", defValue: true, description: "Enabled"},
		"subsys-id":   iscsiCrudFeature{kind: "Int", defValue: 0, description: "Id of the owning subsystem"},
	},
	"port": map[string]iscsiCrudFeature{
		"addr-trtype": iscsiCrudFeature{kind: "String", defValue: "TCP", description: "" +
			AddFlagsEnum(&nvmetPortCreateEnums, "addr-trtype", []string{"TCP", "FC"})},
		"addr-traddr":      iscsiCrudFeature{kind: "String", defValue: "", description: "Listen address"},
		"addr-trsvcid":     iscsiCrudFeature{kind: "String", defValue: "4420", description: "Listen port, TCP only"},
		"inline-data-size": iscsiCrudFeature{kind: "Int", defValue: 0, description: "Inline data size"},
		"max-queue-size":   iscsiCrudFeature{kind: "Int", defValue: 0, description: "Maximum queue size"},
		"pi-enable":        iscsiCrudFeature{kind: "Bool", defValue: false, description: "Protection information"},
		"enabled":          iscsiCrudFeature{kind: "Bool", defValue: true, description: "Enabled"},
	},
	"host": map[string]iscsiCrudFeature{
		"hostnqn":     iscsiCrudFeature{kind: "String", defValue: "", description: "NQN of the initiator"},
		"description": iscsiCrudFeature{kind: "String", defValue: "", description: "Description"},
	},
	"port_subsys": map[string]iscsiCrudFeature{
		"port-id":   iscsiCrudFeature{kind: "Int", defValue: 0, description: "Id of the port"},
		"subsys-id": iscsiCrudFeature{kind: "Int", defValue: 0, description: "Id of the subsystem"},
	},
	"host_subsys": map[string]iscsiCrudFeature{
		"host-id":   iscsiCrudFeature{kind: "Int", defValue: 0, description: "Id of the host"},
		"subsys-id": iscsiCrudFeature{kind: "Int", defValue: 0, description: "Id of the subsystem"},
	},
}

// crudApiPrefix казва в кое API пространство живее дадена категория.
//
// Имената на категориите не се застъпват между двата протокола, затова разпознаването е
// по таблица, а не по подаден параметър — така генеричните функции запазват сигнатурите си
// и iSCSI пътят остава непроменен.
func crudApiPrefix(category string) string {
	if _, ok := nvmetCrudFeatureMap[category]; ok {
		return "nvmet"
	}
	return "iscsi"
}

func crudEndpoint(category string) string {
	return crudApiPrefix(category) + "." + category
}

func crudIdentifiers(category string) []string {
	if v, ok := nvmetCrudIdentifierMap[category]; ok {
		return v
	}
	return iscsiCrudIdentifierMap[category]
}

func crudRequiredAttrs(category string) []string {
	if v, ok := nvmetCrudRequiredAttrMap[category]; ok {
		return v
	}
	return iscsiCrudRequiredAttrMap[category]
}

func crudFeatures(category string) map[string]iscsiCrudFeature {
	if v, ok := nvmetCrudFeatureMap[category]; ok {
		return v
	}
	return iscsiCrudFeatureMap[category]
}

func AddNvmetCrudCommands(parentCmd *cobra.Command) {
	listFormatDesc := AddFlagsEnum(&iscsiCrudListEnums, "format", []string{"csv", "json", "table", "compact"})

	for _, category := range nvmetCrudCategories {
		cmdList := &cobra.Command{
			Use:     "list [terms...]",
			Short:   "List " + category + " entries. Each parameter is a search term for a distinct entry.",
			Aliases: []string{"ls"},
			RunE:    WrapIscsiCrudFunc(iscsiCrudList, category),
		}
		cmdCreate := &cobra.Command{
			Use:   "create",
			Short: "Create a " + category + " entry. Flags are passed to specify the new entry.",
			Args:  cobra.ExactArgs(0),
			RunE:  WrapIscsiCrudFuncNoArgs(iscsiCrudUpdateCreate, category),
		}
		cmdUpdate := &cobra.Command{
			Use:   "update",
			Short: "Update a " + category + " entry, optionally with an id.",
			Args:  cobra.ExactArgs(0),
			RunE:  WrapIscsiCrudFuncNoArgs(iscsiCrudUpdateCreate, category),
		}
		cmdDelete := &cobra.Command{
			Use:     "delete [ids...]",
			Short:   "Delete one or more " + category + " entries by id.",
			Aliases: []string{"rm"},
			RunE:    WrapIscsiCrudFunc(iscsiCrudDelete, category),
		}

		for name, f := range nvmetCrudFeatureMap[category] {
			AddIscsiCrudCommandFlag(cmdCreate, name, f)
			AddIscsiCrudCommandFlag(cmdUpdate, name, f)
		}
		cmdUpdate.Flags().String("id", "", "id of object to update, if not set the object is searched for with the given properties")

		cmdList.Flags().BoolP("json", "j", false, "Equivalent to --format=json")
		cmdList.Flags().BoolP("no-headers", "c", false, "Equivalent to --format=compact. More easily parsed by scripts")
		cmdList.Flags().String("format", "table", "Output table format. Defaults to \"table\" "+listFormatDesc)
		cmdList.Flags().StringP("output", "o", "", "Output property list")
		cmdList.Flags().BoolP("parsable", "p", false, "Show raw values instead of the already parsed values")
		cmdList.Flags().Bool("all", false, "Output all properties")

		cmd := &cobra.Command{Use: category, Short: "Manage nvmet." + category + " entries directly"}
		cmd.AddCommand(cmdList)
		cmd.AddCommand(cmdCreate)
		cmd.AddCommand(cmdUpdate)
		cmd.AddCommand(cmdDelete)
		parentCmd.AddCommand(cmd)
	}
}
