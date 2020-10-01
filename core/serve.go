package core

import (
	"context"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/anz-bank/sysl-go/common"
	"github.com/anz-bank/sysl-go/config"
	"github.com/spf13/afero"
	"gopkg.in/yaml.v2"
)

type serveContextKey int

const (
	serveConfigFileSystemKey serveContextKey = iota
)

// ConfigFileSystemOnto adds a config filesystem to ctx.
func ConfigFileSystemOnto(ctx context.Context, fs afero.Fs) context.Context {
	return context.WithValue(ctx, serveConfigFileSystemKey, fs)
}

// Serve serves an auto-generated service.
//nolint:funlen
func Serve(
	ctx context.Context,
	downstreamConfig, createService, serviceInterface interface{},
	newManager func(cfg *config.DefaultConfig, serviceIntf interface{}, hooks *Hooks) (interface{}, error),
) error {
	MustTypeCheckCreateService(createService, serviceInterface)
	customConfig := NewZeroCustomConfig(reflect.TypeOf(downstreamConfig), GetAppConfigType(createService))
	customConfig, err := LoadCustomConfig(ctx, customConfig)
	if err != nil {
		return err
	}

	customConfigValue := reflect.ValueOf(customConfig).Elem()
	library := customConfigValue.FieldByName("Library").Interface().(config.LibraryConfig)
	genCodeValue := customConfigValue.FieldByName("GenCode")
	appConfig := customConfigValue.FieldByName("App")
	upstream := genCodeValue.FieldByName("Upstream").Interface().(config.UpstreamConfig)
	downstream := genCodeValue.FieldByName("Downstream").Interface()

	defaultConfig := &config.DefaultConfig{
		Library: library,
		GenCode: config.GenCodeConfig{
			Upstream:   upstream,
			Downstream: downstream,
		},
	}

	createServiceResult := reflect.ValueOf(createService).Call(
		[]reflect.Value{reflect.ValueOf(ctx), appConfig},
	)
	if err := createServiceResult[2].Interface(); err != nil {
		return err.(error)
	}
	serviceIntf := createServiceResult[0].Interface()
	hooksIntf := createServiceResult[1].Interface()

	manager, err := newManager(defaultConfig, serviceIntf, hooksIntf.(*Hooks))
	if err != nil {
		return err
	}

	opts := make([]ServerOption, 0)

	switch manager := manager.(type) {
	case Manager: // aka RESTful service manager
		opts = append(opts, WithRestManager(manager))
	case GrpcServerManager:
		opts = append(opts, WithGrpcServerManager(manager))
	default:
		panic(fmt.Errorf("Wrong type returned from newManager()"))
	}

	applicationName := "nameless-autogenerated-app" // TODO source the application name from somewhere
	serverParams := NewServerParams(ctx, applicationName, opts...)
	return serverParams.Start()
}

// LoadCustomConfig populates the given zero customConfig value with configuration data.
func LoadCustomConfig(ctx context.Context, customConfig interface{}) (interface{}, error) {
	// TODO make this more flexible. It should be possible to resolve a config value
	// without needing to access os.Args or hit any kind of filesystem.
	if len(os.Args) != 2 {
		return nil, fmt.Errorf("Wrong number of arguments (usage: %s (config | -h | --help))", os.Args[0])
	}

	if os.Args[1] == "--help" || os.Args[1] == "-h" {
		fmt.Printf("Usage: %s config\n\n", os.Args[0])
		describeCustomConfig(os.Stdout, customConfig)
		fmt.Print("\n\n")
		return nil, nil
	}

	var fs afero.Fs
	if v := ctx.Value(serveConfigFileSystemKey); v != nil {
		fs = v.(afero.Fs)
	} else {
		fs = afero.NewOsFs()
	}

	configPath := os.Args[1]
	configData, err := afero.Afero{Fs: fs}.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	err = yaml.UnmarshalStrict(configData, customConfig)
	return customConfig, err
}

// NewZeroCustomConfig uses reflection to create a new type derived from DefaultConfig,
// but with new GenCode.Downstream and App fields holding the same types as
// downstreamConfig and appConfig. It returns a pointer to a zero value of that
// new type.
func NewZeroCustomConfig(downstreamConfigType, appConfigType reflect.Type) interface{} {
	defaultConfigType := reflect.TypeOf(config.DefaultConfig{})

	libraryField, has := defaultConfigType.FieldByName("Library")
	if !has {
		panic("config.DefaultType missing Library field")
	}

	genCodeType := reflect.TypeOf(config.GenCodeConfig{})

	upstreamField, has := genCodeType.FieldByName("Upstream")
	if !has {
		panic("config.DefaultType missing Upstream field")
	}

	return reflect.New(reflect.StructOf([]reflect.StructField{
		libraryField,
		{Name: "GenCode", Type: reflect.StructOf([]reflect.StructField{
			upstreamField,
			{Name: "Downstream", Type: downstreamConfigType, Tag: `yaml:"downstream"`},
		}), Tag: `yaml:"genCode"`},
		{Name: "App", Type: appConfigType, Tag: `yaml:"app"`},
	})).Interface()
}

// MustTypeCheckCreateService checks that the given createService has an acceptable type, and panics otherwise.
func MustTypeCheckCreateService(createService, serviceInterface interface{}) {
	cs := reflect.TypeOf(createService)
	if cs.NumIn() != 2 {
		panic("createService: wrong number of in params")
	}
	if cs.NumOut() != 3 {
		panic("createService: wrong number of out params")
	}

	var ctx context.Context
	if reflect.TypeOf(&ctx).Elem() != cs.In(0) {
		panic(fmt.Errorf("createService: first in param must be of type context.Context, not %v", cs.In(0)))
	}

	serviceInterfaceType := reflect.TypeOf(serviceInterface)
	if serviceInterfaceType != cs.Out(0) {
		panic(fmt.Errorf("createService: second out param must be of type %v, not %v", serviceInterfaceType, cs.Out(0)))
	}

	var hooks Hooks
	if reflect.TypeOf(&hooks) != cs.Out(1) {
		panic(fmt.Errorf("createService: second out param must be of type *Hooks, not %v", cs.Out(1)))
	}

	var err error
	if reflect.TypeOf(&err).Elem() != cs.Out(2) {
		panic(fmt.Errorf("createService: third out param must be of type error, not %v", cs.Out(1)))
	}
}

// GetAppConfigType extracts the app's config type from createService.
// Precondition: MustTypeCheckCreateService(createService, serviceInterface) succeeded.
func GetAppConfigType(createService interface{}) reflect.Type {
	cs := reflect.TypeOf(createService)
	return cs.In(1)
}

func yamlEgComment(example, format string, args ...interface{}) string {
	return fmt.Sprintf("\033[1;31m%s \033[0;32m# "+format+"\033[0m", append([]interface{}{example}, args...)...)
}

func describeCustomConfig(w io.Writer, customConfig interface{}) {
	commonTypes := map[reflect.Type]string{
		reflect.TypeOf(config.CommonServerConfig{}):   "",
		reflect.TypeOf(config.CommonDownstreamData{}): "",
		reflect.TypeOf(config.TLSConfig{}):            "",
		reflect.TypeOf(common.SensitiveString{}):      yamlEgComment(`"*****"`, "sensitive string"),
	}

	fmt.Fprint(w, "\033[1mConfiguration file YAML schema\033[0m")

	commonTypeNames := make([]string, 0, len(commonTypes))
	commonTypesByName := make(map[string]reflect.Type, len(commonTypes))
	for ct := range commonTypes {
		name := fmt.Sprintf("%s.%s", ct.PkgPath(), ct.Name())
		commonTypeNames = append(commonTypeNames, name)
		commonTypesByName[name] = ct
	}
	sort.Strings(commonTypeNames)

	for _, name := range commonTypeNames {
		ct := commonTypesByName[name]
		if commonTypes[ct] == "" {
			delete(commonTypes, ct)
			fmt.Fprintf(w, "\n\n\033[1;32m%q.%s:\033[0m", ct.PkgPath(), ct.Name())
			describeYAMLForType(w, ct, commonTypes, 4)
			commonTypes[ct] = ""
		}
	}

	fmt.Fprintf(w, "\n\n\033[1mApplication Configuration\033[0m")
	describeYAMLForType(w, reflect.TypeOf(customConfig), commonTypes, 0)
}

func describeYAMLForType(w io.Writer, t reflect.Type, commonTypes map[reflect.Type]string, indent int) {
	outf := func(format string, args ...interface{}) {
		parts := strings.SplitAfterN(format, "\n", 2)
		fmt.Fprintf(w, strings.Join(parts, strings.Repeat(" ", indent)), args...)
	}
	if alias, has := commonTypes[t]; has {
		if alias == "" {
			outf(" " + yamlEgComment(`{}`, "%q.%s", t.PkgPath(), t.Name()))
		} else {
			outf(" %s", alias)
		}
		return
	}
	switch t.Kind() {
	case reflect.Bool:
		outf(" \033[1mfalse\033[0m")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		outf(" \033[1m0\033[0m")
	case reflect.Float32, reflect.Float64:
		outf(" \033[1m0.0\033[0m")
	case reflect.Array, reflect.Slice:
		outf("\n-")
		describeYAMLForType(w, t.Elem(), commonTypes, indent+4)
	case reflect.Interface:
		outf(" " + yamlEgComment("{}", "any value"))
	case reflect.Map:
		outf("\n key: ")
		describeYAMLForType(w, t.Elem(), commonTypes, indent+4)
	case reflect.Ptr:
		describeYAMLForType(w, t.Elem(), commonTypes, indent)
	case reflect.String:
		outf(" \033[1m\"\"\033[0m")
	case reflect.Struct:
		n := t.NumField()
		for i := 0; i < n; i++ {
			f := t.Field(i)
			yamlTag := f.Tag.Get("yaml")
			yamlParts := strings.Split(yamlTag, ",")
			var name string
			if len(yamlParts) > 0 {
				name = yamlParts[0]
			} else {
				name = f.Name
			}
			outf("\n%s:", name)
			describeYAMLForType(w, f.Type, commonTypes, indent+4)
		}
	default:
		panic(fmt.Errorf("describeYAMLForType: Unhandled type: %v", t))
	}
}
