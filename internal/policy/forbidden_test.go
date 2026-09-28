package policy

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// forbiddenList is testdata/forbidden.txt parsed into exact names and
// prefix rules (a trailing "*" in the file).
type forbiddenList struct {
	exact    map[string]bool
	prefixes []string
}

func (f forbiddenList) matches(word string) bool {
	if f.exact[word] {
		return true
	}
	for _, p := range f.prefixes {
		if strings.HasPrefix(word, p) {
			return true
		}
	}
	return false
}

func loadForbidden(t *testing.T) forbiddenList {
	t.Helper()
	data, err := os.ReadFile("testdata/forbidden.txt")
	if err != nil {
		t.Fatalf("read testdata/forbidden.txt: %v", err)
	}
	fl := forbiddenList{exact: map[string]bool{}}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasSuffix(line, "*") {
			fl.prefixes = append(fl.prefixes, strings.TrimSuffix(line, "*"))
			continue
		}
		fl.exact[line] = true
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan testdata/forbidden.txt: %v", err)
	}
	return fl
}

// TestForbiddenListMatchesDocumentedInventory pins testdata/forbidden.txt
// against dev_docs/safety-architecture.md section 3.2 point 2's own merged
// list (itself built from 08-klipper-persistence-guards.md section 5 and
// 10-hazard-analysis.md section 4), so the file and the source documents
// cannot silently drift apart (dev_docs/safety-architecture.md section 3.2
// point 2: "kept as internal/policy/testdata/forbidden.txt with a test
// asserting its size and entries match those tables").
func TestForbiddenListMatchesDocumentedInventory(t *testing.T) {
	fl := loadForbidden(t)

	mustContainExact := []string{
		"SAVE_CONFIG", "CXSAVE_CONFIG", "SAVE_VARIABLE", "SET_IDLE_TIMEOUT",
		"SET_GCODE_OFFSET", "SET_VELOCITY_LIMIT", "SET_PRESSURE_ADVANCE",
		"Qmode", "Qmode_exit", "SET_QMODE_FLAG", "SET_KINEMATIC_POSITION",
		"FORCE_MOVE", "Z_OFFSET_APPLY_PROBE", "PROBE_CALIBRATE", "PID_CALIBRATE",
		"NOZZLE_PID", "NOZZLE_PID_HIGH", "BEDPID", "INPUTSHAPER",
		"BELT_MDL_CALI", "GET_MAX_Z", "Z_FAIL_PROTECT_HOTBED", "TEST_HOME",
		"A_G28_TEST", "EXTRUDE_TEST", "LOAD_AI_T_CMD_TEST", "LOAD_AI_DEAL",
		"CALIBRATE_CUT_POS", "M8200", "FIRMWARE_RESTART", "RESTART", "M112",
		"G28", "G0", "G1", "M84", "TURN_OFF_HEATERS",
	}
	for _, name := range mustContainExact {
		if !fl.exact[name] {
			t.Errorf("testdata/forbidden.txt is missing %q from safety-architecture.md section 3.2 point 2's list", name)
		}
	}

	mustContainPrefix := []string{"EEPROM_", "MOTOR_", "BOX_", "CR_BOX_", "ZDOWN", "BED_MESH_CALIBRATE"}
	for _, want := range mustContainPrefix {
		found := false
		for _, p := range fl.prefixes {
			if p == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("testdata/forbidden.txt is missing the %q* prefix rule", want)
		}
	}

	const minEntries = 30
	if len(fl.exact)+len(fl.prefixes) < minEntries {
		t.Errorf("testdata/forbidden.txt has only %d entries, want at least %d (08 section 5 / 10 section 4 inventory)", len(fl.exact)+len(fl.prefixes), minEntries)
	}
}

// TestForbiddenListNeverContainsAnAllowedTemplateMnemonic guards the other
// direction: the five commands this package's own closed template
// vocabulary renders must never accidentally appear in the forbidden list,
// or the allowlist and the forbidden list would contradict each other.
func TestForbiddenListNeverContainsAnAllowedTemplateMnemonic(t *testing.T) {
	fl := loadForbidden(t)
	for _, mnemonic := range []string{"SET_HEATER_TEMPERATURE", "M106", "M220", "M221", "EXCLUDE_OBJECT"} {
		if fl.matches(mnemonic) {
			t.Errorf("testdata/forbidden.txt forbids %q, which is one of this package's own allowed templates", mnemonic)
		}
	}
}
