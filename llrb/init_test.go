package llrb

import "fmt"
import "testing"

import "github.com/prataprc/storage.go/lib"
import "github.com/prataprc/storage.go/api"
import "github.com/prataprc/storage.go/log"

var _ = fmt.Sprintf("dummy")

func init() {
	setts := map[string]interface{}{
		"log.level": "warn",
		"log.file":  "",
	}
	log.SetLogger(nil, setts)
}

func TestLLRBValidate(t *testing.T) {
	dotest := func(setts lib.Settings) {
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("expected panic")
			}
		}()
		llrb := NewLLRB("test", setts)
		llrb.validateSettings(setts)
	}

	setts := DefaultSettings()
	setts["nodearena.minblock"] = api.MinKeymem - 1
	dotest(setts)

	setts = DefaultSettings()
	setts["nodearena.maxblock"] = api.MaxKeymem + 1
	dotest(setts)

	setts = DefaultSettings()
	setts["nodearena.capacity"] = 0
	dotest(setts)

	setts = DefaultSettings()
	setts["valarena.minblock"] = api.MinValmem - 1
	dotest(setts)

	setts = DefaultSettings()
	setts["valarena.maxblock"] = api.MaxValmem + 1
	dotest(setts)

	setts = DefaultSettings()
	setts["valarena.capacity"] = 0
	dotest(setts)
}
