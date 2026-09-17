package cmd

import (
	"fmt"
	"strings"
	"time"
	"truenas/truenas_incus_ctl/core"

	"github.com/spf13/cobra"
)

// NVMe-oF споделяния през nvmet.* API-то на TrueNAS 26+.
//
// Командите отразяват iSCSI едно към едно — create, activate, deactivate, locate, delete —
// защото това е договорът, който Incus очаква.
//
// Incus 7.4 НЕ знае за NVMe: в целите му конфигурационни метаданни няма нито едно такова
// споменаване, а драйверът `truenas` е обвивка около този бинар (Incus докладва версията
// на драйвера като версията на инструмента). Затова изборът на транспорт живее в ПРОФИЛА
// на config.json, а Incus го избира по име през ключа `truenas.config`. При `transport=nvme`
// командите `share iscsi …` се пренасочват насам — Incus иска път до блоково устройство и
// го получава, без да знае какво има отдолу.
//
// Това е съзнателен компромис: командата се казва iscsi, а прави NVMe. Алтернативата беше
// кръпка в самия Incus, а при „винаги най-новата версия" тя значи прекърпване на всяко
// издание. Затова пренасочването е шумно на всяко място, където се случва.

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

var nvmeActivateCmd = &cobra.Command{
	Use:   "activate <dataset>...",
	Short: "Connect to the NVMe-oF subsystems that map to the given datasets",
	Args:  cobra.MinimumNArgs(1),
}

var nvmeDeactivateCmd = &cobra.Command{
	Use:   "deactivate <dataset>...",
	Short: "Disconnect from the NVMe-oF subsystems that map to the given datasets",
	Args:  cobra.MinimumNArgs(1),
}

var nvmeLocateCmd = &cobra.Command{
	Use:   "locate <dataset>...",
	Short: "Create and/or connect in one call, printing the device path",
	Args:  cobra.MinimumNArgs(1),
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
	nvmeActivateCmd.RunE = WrapCommandFunc(activateNvme)
	nvmeDeactivateCmd.RunE = WrapCommandFunc(deactivateNvme)
	nvmeLocateCmd.RunE = WrapCommandFunc(locateNvme)

	for _, c := range []*cobra.Command{nvmeCreateCmd, nvmeDeleteCmd, nvmeListCmd, nvmeSetupCmd,
		nvmeActivateCmd, nvmeDeactivateCmd, nvmeLocateCmd} {
		c.Flags().StringP("target-prefix", "t", "", "label to prefix the created subsystem name")
		c.Flags().Bool("parsable", false, "Parsable (ie. minimal) output")
	}
	nvmeDeactivateCmd.Flags().Bool("wait", false, "Wait for the device to disappear")
	for _, c := range []*cobra.Command{nvmeLocateCmd} {
		c.Flags().Bool("create", false, "Create the subsystem if missing")
		c.Flags().Bool("activate", false, "Connect after creating")
		c.Flags().Bool("deactivate", false, "Disconnect instead")
		c.Flags().Bool("delete", false, "Delete instead")
		c.Flags().Bool("wait", false, "Wait for the device to disappear on deactivate")
	}
	for _, c := range []*cobra.Command{nvmeCreateCmd, nvmeDeleteCmd, nvmeActivateCmd,
		nvmeDeactivateCmd, nvmeLocateCmd} {
		c.Flags().String("port", "", "NVMe-oF port id or [ip]:[port]. Defaults to the only configured port")
		c.Flags().String("host-nqn", "", "Initiator NQN allowed to access the subsystem. Empty means any host")
	}

	nvmeCmd.AddCommand(nvmeCreateCmd)
	nvmeCmd.AddCommand(nvmeDeleteCmd)
	nvmeCmd.AddCommand(nvmeListCmd)
	nvmeCmd.AddCommand(nvmeSetupCmd)
	nvmeCmd.AddCommand(nvmeActivateCmd)
	nvmeCmd.AddCommand(nvmeDeactivateCmd)
	nvmeCmd.AddCommand(nvmeLocateCmd)
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

	portId, err := LookupNvmePort(api, nvmePortSpec(options))
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

// activateNvme свързва нода към subsystem-а и връща пътя до блоковото устройство.
//
// Това е половината, заради която Incus изобщо може да ползва NVMe: неговият `truenas`
// драйвър не знае за NVMe, но и не му трябва — той пуска тази команда и чака път до
// устройство. Какъв е транспортът отдолу е наша работа.
func activateNvme(cmd *cobra.Command, api core.Session, args []string) error {
	cmd.SilenceUsage = true
	if err := requireRootForNvme("activate"); err != nil {
		return err
	}
	options, _ := GetCobraFlags(cmd, false, nil)
	prefix := strings.TrimSpace(options.allFlags["target_prefix"])
	isParsable := core.IsStringTrue(options.allFlags, "parsable")

	portId, err := LookupNvmePort(api, nvmePortSpec(options))
	if err != nil {
		return err
	}
	addr, port, err := LookupNvmePortAddress(api, portId)
	if err != nil {
		return err
	}

	for _, vol := range args {
		name := nvmeSubsysNameFromVolume(prefix, vol)
		subNqn, err := LookupNvmeSubNqn(api, name)
		if err != nil {
			return err
		}
		if subNqn == "" {
			fmt.Printf("not-found\t%s\n", name)
			continue
		}

		if err := RunNvmeConnect(addr, port, subNqn); err != nil {
			return err
		}

		dev := WaitForNvmeDevice(subNqn, 30*time.Second)
		if dev == "" {
			fmt.Printf("timed-out\t%s\n", subNqn)
			continue
		}

		if isParsable {
			fmt.Println(dev)
		} else {
			fmt.Printf("activated\t%s\n", dev)
		}
	}
	return nil
}

func deactivateNvme(cmd *cobra.Command, api core.Session, args []string) error {
	cmd.SilenceUsage = true
	if err := requireRootForNvme("deactivate"); err != nil {
		return err
	}
	options, _ := GetCobraFlags(cmd, false, nil)
	prefix := strings.TrimSpace(options.allFlags["target_prefix"])
	shouldWait := core.IsStringTrue(options.allFlags, "wait")

	for _, vol := range args {
		name := nvmeSubsysNameFromVolume(prefix, vol)
		subNqn, err := LookupNvmeSubNqn(api, name)
		if err != nil {
			return err
		}
		if subNqn == "" {
			fmt.Printf("not-found\t%s\n", name)
			continue
		}

		if err := RunNvmeDisconnect(subNqn); err != nil {
			return err
		}
		if shouldWait && !WaitForNvmeDeviceGone(subNqn, 30*time.Second) {
			fmt.Printf("timed-out\t%s\n", subNqn)
			continue
		}
		fmt.Printf("deactivated\t%s\n", subNqn)
	}
	return nil
}

// locateNvme е „всичко наведнъж" — Incus ползва точно нея при закачане на том.
func locateNvme(cmd *cobra.Command, api core.Session, args []string) error {
	cmd.SilenceUsage = true
	options, _ := GetCobraFlags(cmd, false, nil)

	if core.IsStringTrue(options.allFlags, "create") {
		if err := createNvme(cmd, api, args); err != nil {
			return err
		}
	}
	if core.IsStringTrue(options.allFlags, "delete") {
		return deleteNvme(cmd, api, args)
	}
	if core.IsStringTrue(options.allFlags, "deactivate") {
		return deactivateNvme(cmd, api, args)
	}
	if core.IsStringTrue(options.allFlags, "activate") {
		return activateNvme(cmd, api, args)
	}
	return nil
}

// nvmePortSpec приема и двете имена на флага.
//
// Собствените nvme команди го наричат `--port`, но когато Incus вика командата през
// `share iscsi`, там флагът е `--portal`. Една функция обслужва двете повърхности,
// вместо да се дублира логиката.
func nvmePortSpec(options FlagMap) string {
	if v := strings.TrimSpace(options.allFlags["port"]); v != "" {
		return v
	}

	// `--portal` по подразбиране е ":" при iSCSI — „адресът на хоста, портът по
	// подразбиране". За NVMe това не значи нищо и подадено както е дава
	// „no NVMe-oF port matches", вместо да се вземе единственият конфигуриран порт.
	portal := strings.TrimSpace(options.allFlags["portal"])
	if strings.Trim(portal, ":[] ") == "" {
		return ""
	}
	return portal
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

// isNvmeTransport казва дали профилът е конфигуриран за NVMe-oF вместо iSCSI.
//
// Проверява се в НАЧАЛОТО на всяка iSCSI команда, която Incus вика. Дотам конфигурацията
// вече е прочетена (InitializeApiClient върви в WrapCommandFunc преди самата функция).
func isNvmeTransport() bool {
	return g_transport == "nvme"
}

// dispatchNvme пренасочва iSCSI команда към NVMe и го КАЗВА в дебъг дневника.
//
// Мълчаливото пренасочване е рецепта за изгубен половин ден: човек чете `share iscsi
// activate` в дневника на Incus, търси iSCSI сесия и не намира нищо.
func dispatchNvme(verb string, fn func(*cobra.Command, core.Session, []string) error,
	cmd *cobra.Command, api core.Session, args []string) error {
	DebugString("transport=nvme: \"share iscsi " + verb + "\" is being served over NVMe-oF")
	return fn(cmd, api, args)
}
