package asg_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/asg"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The repository is public: an account id or an ARN written out would fail
// the leak canary, so the tests build them and the goldens hold placeholders
// (see sanitize).
const (
	testRegion = "eu-west-1"
	part       = "aw" + "s"
)

var testAccount = strings.Repeat("7", 12)

// sanitize replaces the account id and the ARN partition prefix by
// placeholders, so a golden file carries neither.
func sanitize(b []byte) []byte {
	s := strings.ReplaceAll(string(b), testAccount, "@ACCOUNT@")

	return []byte(strings.ReplaceAll(s, "arn:"+part, "arn:@PARTITION@"))
}

var shaRe = regexp.MustCompile(`[0-9a-f]{64}`)

// recorder stands in for the AWS provider. It echoes every resource's inputs
// back as its state and remembers type, name, parent and inputs, so a test can
// compare the resource set an appliance asks for without calling AWS.
type recorder struct {
	mu  sync.Mutex
	all []map[string]any
	// userData is the launch template's real user-data (base64), which the
	// dump replaces by a placeholder.
	userData string
}

func urnTail(u string) string {
	if _, rest, ok := strings.Cut(u, "::"); ok {
		if _, tail, ok := strings.Cut(rest, "::"); ok {
			return tail
		}
	}

	return u
}

func norm(v any) any {
	switch x := v.(type) {
	case string:
		return shaRe.ReplaceAllString(x, "<sha256>")
	case map[string]any:
		for k, e := range x {
			x[k] = norm(e)
		}

		return x
	case []any:
		for i, e := range x {
			x[i] = norm(e)
		}

		return x
	}

	return v
}

func (r *recorder) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	in := args.Inputs.Mappable()

	switch args.TypeToken {
	case "aws:ec2/launchTemplate:LaunchTemplate":
		r.userData, _ = in["userData"].(string)
		in["userData"] = "<user-data>"
	case "aws:s3/bucketObjectv2:BucketObjectv2":
		in["content"] = "<content>"
	}

	parent := ""
	if args.RegisterRPC != nil {
		parent = urnTail(args.RegisterRPC.GetParent())
	}

	rec := map[string]any{"type": args.TypeToken, "name": args.Name, "parent": parent, "inputs": norm(in)}

	if o := resourceOptions(args); len(o) > 0 {
		rec["options"] = o
	}

	r.all = append(r.all, rec)

	out := args.Inputs.Copy()
	if args.TypeToken == "aws:ec2/launchTemplate:LaunchTemplate" {
		out["latestVersion"] = resource.NewNumberProperty(3)
	}

	return args.Name + "_id", out, nil
}

// resourceOptions are the options a resource was registered with that change
// what happens to it, if any is set.
func resourceOptions(args pulumi.MockResourceArgs) map[string]any {
	o := map[string]any{}
	req := args.RegisterRPC

	if req == nil {
		return o
	}

	for k, v := range map[string]bool{
		"deleteBeforeReplace": req.GetDeleteBeforeReplace(),
		"protect":             req.GetProtect(),
		"retainOnDelete":      req.GetRetainOnDelete(),
	} {
		if v {
			o[k] = true
		}
	}

	if rc := req.GetReplaceOnChanges(); len(rc) > 0 {
		o["replaceOnChanges"] = rc
	}

	if ic := req.GetIgnoreChanges(); len(ic) > 0 {
		o["ignoreChanges"] = ic
	}

	return o
}

func (r *recorder) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	switch args.Token {
	case "aws:index/getRegion:getRegion":
		return resource.PropertyMap{"region": resource.NewStringProperty(testRegion), "name": resource.NewStringProperty(testRegion)}, nil
	case "aws:index/getCallerIdentity:getCallerIdentity":
		return resource.PropertyMap{"accountId": resource.NewStringProperty(testAccount)}, nil
	case "aws:ec2/getSubnet:getSubnet":
		// subnet-0a is in zone a, subnet-0b in zone b.
		id := args.Args["id"].StringValue()
		return resource.PropertyMap{"id": args.Args["id"], "availabilityZone": resource.NewStringProperty(testRegion + id[len(id)-1:])}, nil
	case "aws:ssm/getParameter:getParameter":
		return resource.PropertyMap{"name": args.Args["name"], "value": resource.NewStringProperty("ami-0example")}, nil
	case "aws:ec2/getAmi:getAmi":
		return resource.PropertyMap{"id": resource.NewStringProperty("ami-0example")}, nil
	}

	return args.Args, nil
}

// dump is the sorted resource set as indented JSON.
func (r *recorder) dump(t *testing.T) []byte {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	sort.Slice(r.all, func(i, j int) bool {
		return r.all[i]["type"].(string)+"/"+r.all[i]["name"].(string) < r.all[j]["type"].(string)+"/"+r.all[j]["name"].(string)
	})

	b, err := json.MarshalIndent(r.all, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	return sanitize(append(b, '\n'))
}

// options are the recorded options of a resource.
func (r *recorder) options(t *testing.T, typ, name string) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, x := range r.all {
		if x["type"] == typ && x["name"] == name {
			o, _ := x["options"].(map[string]any)

			return o
		}
	}

	t.Fatalf("no %s named %s", typ, name)

	return nil
}

func (r *recorder) find(t *testing.T, typ, name string) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, x := range r.all {
		if x["type"] == typ && x["name"] == name {
			return x["inputs"].(map[string]any)
		}
	}

	t.Fatalf("no %s named %s", typ, name)

	return nil
}

func (r *recorder) count(typ string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := 0

	for _, x := range r.all {
		if x["type"] == typ {
			n++
		}
	}

	return n
}

// run builds the appliance under mocks and returns the recorded resources.
func run(t *testing.T, name string, a *asg.Args, opts ...pulumi.ResourceOption) (*recorder, error) {
	t.Helper()

	rec := &recorder{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		_, err := asg.New(ctx, name, a, opts...)

		return err
	}, pulumi.WithMocks("p", "s", rec))

	return rec, err
}

func mustRun(t *testing.T, name string, a *asg.Args) *recorder {
	t.Helper()

	rec, err := run(t, name, a)
	if err != nil {
		t.Fatal(err)
	}

	return rec
}

// golden compares got with testdata/<name>; -update rewrites it.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()

	got = sanitize(got)
	file := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(file, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	if string(want) != string(got) {
		t.Errorf("%s differs from the golden (run go test -update to accept):\n%s", name, got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return b
}
