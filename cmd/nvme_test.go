package cmd

import (
	"strings"
	"testing"
)

// Именуването решава дали повторно пускане ще намери заварения subsystem, или ще опита да
// създаде втори. Затова е заковано с тестове, а не оставено на четене.

func TestNvmeSubsysNameFromVolume(t *testing.T) {
	cases := []struct {
		prefix, vol, want string
	}{
		{"", "tes/zz-nvme-test", "tes-zz-nvme-test"},
		{"", "pool/deep/path", "pool-deep-path"},
		{"node1", "tes/vol", "node1-tes-vol"},
		{"", "Pool/MiXeD", "pool-mixed"},
		{"", "tes/vol@snap", "tes-vol-snap"},
		{"", "tes/vol_with.dots", "tes-vol-with-dots"},
		// Водещите и крайните разделители отпадат — иначе NQN-ът завършва с тире.
		{"", "/tes/vol/", "tes-vol"},
	}
	for _, c := range cases {
		if got := nvmeSubsysNameFromVolume(c.prefix, c.vol); got != c.want {
			t.Errorf("nvmeSubsysNameFromVolume(%q, %q) = %q, искахме %q", c.prefix, c.vol, got, c.want)
		}
	}
}

func TestNvmeDevicePath(t *testing.T) {
	cases := []struct{ vol, want string }{
		{"tes/vol", "zvol/tes/vol"},
		{"zvol/tes/vol", "zvol/tes/vol"},
		{"/dev/zvol/tes/vol", "/dev/zvol/tes/vol"},
	}
	for _, c := range cases {
		if got := nvmeDevicePath(c.vol); got != c.want {
			t.Errorf("nvmeDevicePath(%q) = %q, искахме %q", c.vol, got, c.want)
		}
	}
}

// NQN-ът е до 223 знака. Под тази граница нищо не се хешира — иначе имената стават
// нечетими без причина.
func TestMakeNvmeSubNqnStaysReadableUnderLimit(t *testing.T) {
	nqn := MakeNvmeSubNqn("", "tes/zz-nvme-test")
	if !strings.HasSuffix(nqn, ":tes-zz-nvme-test") {
		t.Errorf("очаквахме четимо име, получихме %q", nqn)
	}
	if len(nqn) > MAX_NQN_LENGTH {
		t.Errorf("NQN е %d знака, границата е %d", len(nqn), MAX_NQN_LENGTH)
	}
}

func TestMakeNvmeSubNqnHashesWhenTooLong(t *testing.T) {
	long := "tes/" + strings.Repeat("abcdefghij/", 30)
	nqn := MakeNvmeSubNqn("", long)
	if len(nqn) > MAX_NQN_LENGTH {
		t.Errorf("NQN е %d знака, границата е %d", len(nqn), MAX_NQN_LENGTH)
	}
	if !strings.HasPrefix(nqn, "nqn.2025-06.bg.fiberhost:") {
		t.Errorf("хешираният NQN изгуби представката: %q", nqn)
	}
}

// Празен hostnqn значи „който и да е" — тогава изобщо не се прави host запис.
func TestLookupNvmeHostOrCreateSkipsOnEmpty(t *testing.T) {
	api := SetupMultiTest(t, []string{"няма да се вика"}, []string{"{}"}, "")
	id, err := LookupNvmeHostOrCreate(api, "")
	if err != nil {
		t.Fatalf("неочаквана грешка: %v", err)
	}
	if id != -1 {
		t.Errorf("при празен hostnqn очаквахме -1, получихме %d", id)
	}
	if api.callIdx != 0 {
		t.Errorf("не биваше да се прави заявка, а бяха %d", api.callIdx)
	}
}

// Празният порт спец без нито един конфигуриран порт трябва да даде ясна грешка, а не
// мълчаливо да продължи с -1.
func TestLookupNvmePortFailsWhenNoneConfigured(t *testing.T) {
	api := SetupMultiTest(t,
		[]string{`[[],{}]`},
		[]string{`{"result":[]}`},
		"")
	_, err := LookupNvmePort(api, "")
	if err == nil {
		t.Fatal("очаквахме грешка, когато няма конфигуриран порт")
	}
	if !strings.Contains(err.Error(), "no NVMe-oF port") {
		t.Errorf("неясно съобщение: %v", err)
	}
}
