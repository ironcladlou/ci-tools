package testhelper

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/pmezard/go-difflib/difflib"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// WriteToFixture reads an input fixture file and returns the data
func WriteToFixture(t *testing.T, identifier string, data []byte) {
	golden, err := golden(t, &Options{Suffix: identifier})
	if err != nil {
		t.Fatalf("failed to get absolute path to testdata file: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(golden), 0755); err != nil {
		t.Fatalf("failed to create fixture directory: %v", err)
	}
	if err := ioutil.WriteFile(golden, data, 0644); err != nil {
		t.Fatalf("failed to write testdata file: %v", err)
	}
}

// ReadFromFixture reads an input fixture file and returns the data
func ReadFromFixture(t *testing.T, identifier string) []byte {
	golden, err := golden(t, &Options{Suffix: identifier})
	if err != nil {
		t.Fatalf("failed to get absolute path to testdata file: %v", err)
	}

	data, err := ioutil.ReadFile(golden)
	if err != nil {
		t.Fatalf("failed to read testdata file: %v", err)
	}
	return data
}

type Options struct {
	Prefix string
	Suffix string
}

type Option func(*Options)

func WithPrefix(prefix string) Option {
	return func(o *Options) {
		o.Prefix = prefix
	}
}

func WithSuffix(suffix string) Option {
	return func(o *Options) {
		o.Suffix = suffix
	}
}

// golden determines the golden file to use
func golden(t *testing.T, opts *Options) (string, error) {
	return filepath.Abs(filepath.Join("testdata", sanitizeFilename(opts.Prefix+t.Name()+opts.Suffix)) + ".yaml")
}

// CompareWithFixture will compare output with a test fixture and allows to automatically update them
// by setting the UPDATE env var.
// If output is not a []byte or string, it will get serialized as yaml prior to the comparison.
// The fixtures are stored in $PWD/testdata/prefix${testName}.yaml
func CompareWithFixture(t *testing.T, output interface{}, opts ...Option) {
	t.Helper()
	options := &Options{}
	for _, opt := range opts {
		opt(options)
	}

	var serializedOutput []byte
	switch v := output.(type) {
	case []byte:
		serializedOutput = v
	case string:
		serializedOutput = []byte(v)
	default:
		serialized, err := yaml.Marshal(v)
		if err != nil {
			t.Fatalf("failed to yaml marshal output of type %T: %v", output, err)
		}
		serializedOutput = serialized
	}

	golden, err := golden(t, options)
	if err != nil {
		t.Fatalf("failed to get absolute path to testdata file: %v", err)
	}
	if os.Getenv("UPDATE") != "" {
		if err := os.MkdirAll(filepath.Dir(golden), 0755); err != nil {
			t.Fatalf("failed to create fixture directory: %v", err)
		}
		if err := ioutil.WriteFile(golden, serializedOutput, 0644); err != nil {
			t.Fatalf("failed to write updated fixture: %v", err)
		}
	}
	expected, err := ioutil.ReadFile(golden)
	if err != nil {
		t.Fatalf("failed to read testdata file: %v", err)
	}

	diff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(expected)),
		B:        difflib.SplitLines(string(serializedOutput)),
		FromFile: golden,
		ToFile:   "Current",
		Context:  3,
	}
	diffStr, err := difflib.GetUnifiedDiffString(diff)
	if err != nil {
		t.Fatal(err)
	}

	if diffStr != "" {
		t.Errorf("got diff between expected and actual result: \n%s\n\nIf this is expected, re-run the test with `UPDATE=true go test ./...` to update the fixtures.", diffStr)
	}
}

func sanitizeFilename(s string) string {
	result := strings.Builder{}
	for _, r := range s {
		if (r >= 'a' && r < 'z') || (r >= 'A' && r < 'Z') || r == '_' || r == '.' || (r >= '0' && r <= '9') {
			// The thing is documented as returning a nil error so lets just drop it
			_, _ = result.WriteRune(r)
			continue
		}
		if !strings.HasSuffix(result.String(), "_") {
			result.WriteRune('_')
		}
	}
	return "zz_fixture_" + result.String()
}

var (
	// EquateErrorMessage reports errors to be equal if both are nil
	// or both have the same message.
	//https://github.com/google/go-cmp/issues/24#issuecomment-317635190
	EquateErrorMessage = cmp.Comparer(func(x, y error) bool {
		if x == nil || y == nil {
			return x == nil && y == nil
		}
		return x.Error() == y.Error()
	})

	// RuntimObjectIgnoreRvTypeMeta compares two kubernetes objects, ignoring their resource
	// version and TypeMeta. It is what you want 99% of the time.
	RuntimObjectIgnoreRvTypeMeta = cmp.Comparer(func(x, y runtime.Object) bool {
		xCopy := x.DeepCopyObject()
		yCopy := y.DeepCopyObject()
		cleanRVAndTypeMeta(xCopy)
		cleanRVAndTypeMeta(yCopy)
		return cmp.Diff(xCopy, yCopy) == ""
	})
)

func cleanRVAndTypeMeta(r runtime.Object) {
	if metaObject, ok := r.(metav1.Object); ok {
		metaObject.SetResourceVersion("")
	}
	if typeObject, ok := r.(interface{ SetGroupVersionKind(schema.GroupVersionKind) }); ok {
		typeObject.SetGroupVersionKind(schema.GroupVersionKind{})
	}
}
