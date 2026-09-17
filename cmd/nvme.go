package cmd

import (
	"fmt"
	"strings"
	"truenas/truenas_incus_ctl/core"

	"github.com/spf13/cobra"
)

// NVMe-oF споделяния през nvmet.* API-то на TrueNAS 26+.
//
// Защо няма activate/deactivate/locate, каквито има при iSCSI: те са per-том и имат смисъл
// само ако Incus вика инструмента за всеки том. Incus 7.4 говори с TrueNAS САМО по iSCSI —
// в целите му метаданни няма нито едно nvme споменаване. Тук таргетът се сглобява веднъж и
// нодовете се закачат за него сами (nvme connect + LVM отгоре), затова командите са
// таргет-ориентирани, не том-ориентирани.

var nvmeCmd = &cobra.Command{
	Use:   "nvme",
	Short: "Create, list or delete NVMe-oF subsystems that map to the given datasets",
}

var nvmeCreateCmd = &cobra.Command{
	Use:   "create <dataset>...",
	Short: "Create NVMe-oF subsystems and namespaces that map to the given datasets",
	Args:  cobra.MinimumNArgs(1),
}

var nvmeDeleteCmd = &cobra.Command{
	Use:   "delete <dataset>...",
	Short: "Delete the NVMe-oF subsystems that map to the given datasets",
	Args:  cobra.MinimumNArgs(1),
}

var nvmeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List NVMe-oF subsystems with their namespaces",
	Args:  cobra.ExactArgs(0),
}

var nvmeSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Verify that the NVMe-oF service is running and a port is configured",
	Args:  cobra.ExactArgs(0),
}

func init() {
	nvmeCreateCmd.RunE = WrapCommandFunc(createNvme)
	nvmeDeleteCmd.RunE = WrapCommandFunc(deleteNvme)
	nvmeListCmd.RunE = WrapCommandFunc(listNvme)
	nvmeSetupCmd.RunE = WrapCommandFunc(setupNvme)

	for _, c := range []*cobra.Command{nvmeCreateCmd, nvmeDeleteCmd, nvmeListCmd, nvmeSetupCmd} {
		c.Flags().StringP("target-prefix", "t", "", "label to prefix the created subsystem name")
		c.Flags().Bool("parsable", false, "Parsable (ie. minimal) output")
	}
	for _, c := range []*cobra.Command{nvmeCreateCmd, nvmeDeleteCmd} {
		c.Flags().String("port", "", "NVMe-oF port id or [ip]:[port]. Defaults to the only configured port")
		c.Flags().String("host-nqn", "", "Initiator NQN allowed to access the subsystem. Empty means any host")
	}

	nvmeCmd.AddCommand(nvmeCreateCmd)
	nvmeCmd.AddCommand(nvmeDeleteCmd)
	nvmeCmd.AddCommand(nvmeListCmd)
	nvmeCmd.AddCommand(nvmeSetupCmd)
	AddNvmetCrudCommands(nvmeCmd)

	shareCmd.AddCommand(nvmeCmd)
}

// nvmeSubsysNameFromVolume прави името на subsystem-а от пътя на тома.
//
// TrueNAS сглобява пълния subnqn от `basenqn` + това име, затова тук се подава само
// името — така NQN-ът остава последователен с останалите споделяния на уреда, вместо
// да съжителстват два формата.
func nvmeSubsysNameFromVolume(prefix, vol string) string {
	name := strings.ToLower(vol)
	name = strings.NewReplacer("/", "-", "@", "-", ":", "-", "_", "-", ".", "-").Replace(name)
	name = strings.Trim(name, "-")
	if prefix != "" {
		name = prefix + "-" + name
	}
	return name
}

func nvmeDevicePath(vol string) string {
	if strings.HasPrefix(vol, "zvol/") || strings.HasPrefix(vol, "/") {
		return vol
	}
	return "zvol/" + vol
}

func createNvme(cmd *cobra.Command, api core.Session, args []string) error {
	cmd.SilenceUsage = true
	options, _ := GetCobraFlags(cmd, false, nil)

	prefix := strings.TrimSpace(options.allFlags["target_prefix"])
	hostNqn := strings.TrimSpace(options.allFlags["host_nqn"])
	isParsable := core.IsStringTrue(options.allFlags, "parsable")

	// Всяко създадено нещо влиза тук и се отменя при провал — включително двете join
	// таблици. При iSCSI targetextent.create НЕ се записва (iscsi.go:350-355) и остава
	// сирак, ако следващата стъпка гръмне.
	changes := make([]typeApiCallRecord, 0)
	defer func() { undoIscsiCreateList(api, &changes) }()

	portId, err := LookupNvmePort(api, options.allFlags["port"])
	if err != nil {
		return err
	}

	hostId := -1
	if hostNqn != "" {
		if hostId, err = LookupNvmeHostOrCreate(api, hostNqn); err != nil {
			return err
		}
		if hostId == -1 {
			return fmt.Errorf("could not find or create an NVMe-oF host for %q", hostNqn)
		}
	}

	created := make([]string, 0, len(args))

	for _, vol := range args {
		name := nvmeSubsysNameFromVolume(prefix, vol)
		devPath := nvmeDevicePath(vol)

		// 1. subsystem — по име. Без ранния изход на iscsi.go:194-199: дори когато
		// subsystem-ът вече е налице, връзките и namespace-ът се сверяват, за да се
		// самолекува полусъздадено състояние.
		subsysId, err := lookupNvmeIdByFilter(api, "nvmet.subsys",
			[]interface{}{[]interface{}{"name", "=", name}})
		if err != nil {
			return err
		}
		if subsysId == -1 {
			obj := map[string]interface{}{
				"name":           name,
				"allow_any_host": hostNqn == "",
			}
			if subsysId, err = createNvmeObject(api, "nvmet.subsys", obj); err != nil {
				return err
			}
			if subsysId == -1 {
				return fmt.Errorf("could not create an NVMe-oF subsystem named %q", name)
			}
			changes = append(changes, typeApiCallRecord{
				endpoint:   "nvmet.subsys.create",
				resultList: []interface{}{map[string]interface{}{"id": subsysId}},
			})
		}

		// 2. порт ↔ subsystem
		linkId, isNew, err := EnsureNvmeLink(api, "nvmet.port_subsys",
			map[string]interface{}{"port_id": portId, "subsys_id": subsysId})
		if err != nil {
			return err
		}
		if isNew {
			changes = append(changes, typeApiCallRecord{
				endpoint:   "nvmet.port_subsys.create",
				resultList: []interface{}{map[string]interface{}{"id": linkId}},
			})
		}

		// 3. host ↔ subsystem, само при зададен hostnqn
		if hostId != -1 {
			linkId, isNew, err = EnsureNvmeLink(api, "nvmet.host_subsys",
				map[string]interface{}{"host_id": hostId, "subsys_id": subsysId})
			if err != nil {
				return err
			}
			if isNew {
				changes = append(changes, typeApiCallRecord{
					endpoint:   "nvmet.host_subsys.create",
					resultList: []interface{}{map[string]interface{}{"id": linkId}},
				})
			}
		}

		// 4. namespace — филтрира се по subsystem И път, защото един и същ zvol може да
		// е изнесен в няколко subsystem-а.
		//
		// Полето е `subsys.id`, НЕ `subsys_id`. Второто се приема при СЪЗДАВАНЕ, но при
		// заявка връща нула записа, без да се оплаче — проверката за съществуващ namespace
		// винаги казваше „няма" и всяко повторно пускане опитваше дубликат. Отговорът
		// разгръща връзката в обект (`"subsys": {...}`), затова и филтърът е по пътя в него.
		nsId, err := lookupNvmeIdByFilter(api, "nvmet.namespace", []interface{}{
			[]interface{}{"subsys.id", "=", subsysId},
			[]interface{}{"device_path", "=", devPath},
		})
		if err != nil {
			return err
		}
		if nsId == -1 {
			obj := map[string]interface{}{
				"device_type": "ZVOL",
				"device_path": devPath,
				"subsys_id":   subsysId,
			}
			if nsId, err = createNvmeObject(api, "nvmet.namespace", obj); err != nil {
				return err
			}
			if nsId == -1 {
				return fmt.Errorf("could not create an NVMe-oF namespace for %q", devPath)
			}
			changes = append(changes, typeApiCallRecord{
				endpoint:   "nvmet.namespace.create",
				resultList: []interface{}{map[string]interface{}{"id": nsId}},
			})
		}

		created = append(created, name)
	}

	// COMMIT — оттук нататък нищо не се отменя.
	changes = nil

	for _, name := range created {
		if isParsable {
			fmt.Println(name)
		} else {
			fmt.Printf("created\t%s\n", name)
		}
	}
	return nil
}

func deleteNvme(cmd *cobra.Command, api core.Session, args []string) error {
	cmd.SilenceUsage = true
	options, _ := GetCobraFlags(cmd, false, nil)

	prefix := strings.TrimSpace(options.allFlags["target_prefix"])
	isParsable := core.IsStringTrue(options.allFlags, "parsable")

	for _, vol := range args {
		name := nvmeSubsysNameFromVolume(prefix, vol)

		subsysId, err := lookupNvmeIdByFilter(api, "nvmet.subsys",
			[]interface{}{[]interface{}{"name", "=", name}})
		if err != nil {
			return err
		}
		if subsysId == -1 {
			fmt.Printf("not-found\t%s\n", name)
			continue
		}

		// Редът има значение: namespace-ите и връзките държат subsystem-а зает.
		// `subsys.id` при namespace — вж. бележката в createNvme.
		if err = deleteNvmeChildren(api, "nvmet.namespace", "subsys.id", subsysId); err != nil {
			return err
		}
		if err = deleteNvmeChildren(api, "nvmet.host_subsys", "subsys_id", subsysId); err != nil {
			return err
		}
		if err = deleteNvmeChildren(api, "nvmet.port_subsys", "subsys_id", subsysId); err != nil {
			return err
		}

		if _, err = core.ApiCall(api, "nvmet.subsys.delete", defaultCallTimeout,
			[]interface{}{subsysId}); err != nil {
			return err
		}

		if isParsable {
			fmt.Println(name)
		} else {
			fmt.Printf("deleted\t%s\n", name)
		}
	}
	return nil
}

// deleteNvmeChildren трие всички записи в endpoint, чието поле `key` сочи към id.
//
// Филтрира се на сървъра — вж. queryNvmeIdsByFilter.
func deleteNvmeChildren(api core.Session, endpoint, key string, id int) error {
	ids, err := queryNvmeIdsByFilter(api, endpoint,
		[]interface{}{[]interface{}{key, "=", id}})
	if err != nil {
		return err
	}

	for _, rowId := range ids {
		if _, err := core.ApiCall(api, endpoint+".delete", defaultCallTimeout,
			[]interface{}{rowId}); err != nil {
			return err
		}
	}
	return nil
}

func listNvme(cmd *cobra.Command, api core.Session, args []string) error {
	cmd.SilenceUsage = true

	subsys, err := QueryApi(api, "nvmet.subsys", nil, nil, nil, typeQueryParams{})
	if err != nil {
		return err
	}

	// Отговорът разгръща връзката в обект (`"subsys": {...}`), а insertProperties
	// (util_common.go:424) свежда вложен обект до nil, освен ако valueOrder не каже кой
	// ключ да извади. С "id" връзката става точно идентификатора си.
	nsParams := typeQueryParams{valueOrder: []string{"id"}}
	namespaces, err := QueryApi(api, "nvmet.namespace", nil, nil, nil, nsParams)
	if err != nil {
		return err
	}

	nsBySubsys := make(map[string][]string)
	for _, ns := range GetListFromQueryResponse(&namespaces) {
		key := fmt.Sprint(ns["subsys"])
		nsBySubsys[key] = append(nsBySubsys[key], fmt.Sprint(ns["device_path"]))
	}

	for _, s := range GetListFromQueryResponse(&subsys) {
		paths := nsBySubsys[fmt.Sprint(s["id"])]
		fmt.Printf("%v\t%v\t%s\n", s["name"], s["subnqn"], strings.Join(paths, ","))
	}
	return nil
}

func setupNvme(cmd *cobra.Command, api core.Session, args []string) error {
	cmd.SilenceUsage = true

	msg, err := CheckRemoteNvmetServiceIsRunning(api)
	if err != nil {
		return err
	}
	if msg != "" {
		return fmt.Errorf("%s", msg)
	}

	portId, err := LookupNvmePort(api, "")
	if err != nil {
		return err
	}

	fmt.Printf("Port ID: %d\n", portId)
	return nil
}
