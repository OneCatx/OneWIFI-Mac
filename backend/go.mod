module proletarius.local/core

go 1.26.3

// quectel-qmi-go is vendored (third_party) so we can inject a libusb-based
// raw QMI transport on macOS (darwin), where /dev/cdc-wdmX does not exist.
// The upstream package keeps openRawTransportHook private, so the only way
// to add a darwin transport is to vendor the module and add a darwin file.
replace github.com/iniwex5/quectel-qmi-go => ./third_party/quectel-qmi-go

// swu-go v0.0.1 ships Linux XFRM/serial sources without build constraints.
// Keep the audited version locally so reader VoWiFi can use its portable
// userspace netstack on macOS while retaining the Linux implementations.
replace github.com/1239t/swu-go => ./third_party/swu-go

// Keep the pinned euicc-go API; fix STORE DATA final-block framing locally.
// Provenance and the narrow patch are documented in third_party/euicc-go/UPSTREAM.md.
replace github.com/damonto/euicc-go => ./third_party/euicc-go

// Reset and enumeration share uicc-go's existing PC/SC library reference count.
replace github.com/damonto/uicc-go => ./third_party/uicc-go

require (
	github.com/1239t/swu-go v0.0.1
	github.com/damonto/euicc-go v1.1.3-0.20260628013808-8d873a2dfc98
	github.com/damonto/uicc-go v0.0.0-20260629073618-7ddada6bb13e
	github.com/emiago/sipgo v1.5.0
	github.com/google/uuid v1.6.0
	github.com/icholy/digest v1.1.0
	github.com/iniwex5/quectel-qmi-go v0.6.0
	github.com/lestrrat-go/file-rotatelogs v2.4.0+incompatible
	github.com/spf13/viper v1.21.0
	github.com/warthog618/sms v0.3.0
	go.bug.st/serial v1.6.4
	go.uber.org/zap v1.27.1
	go.yaml.in/yaml/v3 v3.0.4
	golang.org/x/sync v0.20.0
	golang.org/x/text v0.36.0
	gvisor.dev/gvisor v0.0.0-20240521174809-5eedbf551134
)

require (
	github.com/ElMostafaIdrassi/goscard v1.0.0
	github.com/creack/goselect v0.1.2 // indirect
	github.com/ebitengine/purego v0.10.0 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.4.0 // indirect
	github.com/gobwas/httphead v0.1.0 // indirect
	github.com/gobwas/pool v0.2.1 // indirect
	github.com/gobwas/ws v1.3.2 // indirect
	github.com/google/btree v1.1.2 // indirect
	github.com/iniwex5/netlink v1.3.3 // indirect
	github.com/jonboulle/clockwork v0.5.0 // indirect
	github.com/lestrrat-go/strftime v1.2.0 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/sagikazarmark/locafero v0.11.0 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/songgao/water v0.0.0-20200317203138-2b4b6d7c09d8 // indirect
	github.com/sourcegraph/conc v0.3.1-0.20240121214520-5f936abd7ae8 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/time v0.5.0 // indirect
)
