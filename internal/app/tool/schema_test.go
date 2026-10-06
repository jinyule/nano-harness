package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type todoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

type fullArguments struct {
	FilePath string     `json:"file_path"`
	Limit    *float64   `json:"limit"`
	All      *bool      `json:"replace_all"`
	Todos    []todoItem `json:"todos"`
	Meta     *struct {
		Note string `json:"note"`
	} `json:"meta"`
}

func fullParameters() Parameters {
	return Parameters{
		Required("file_path", String("Path to read. Provide `file_path` before <content> & more.")),
		Optional("limit", Number("Maximum lines.")),
		Optional("replace_all", Boolean("")),
		Optional("todos", Array("The complete list.", Object("", false,
			Required("content", String("What.")),
			Required("status", String("State.", "pending", "in_progress", "completed")),
		))),
		Optional("meta", Object("Open metadata.", true, Optional("note", String("")))),
	}
}

func TestParameters_MarshalMatchesUpstreamKeyOrder(t *testing.T) {
	got := string(fullParameters().marshal())
	want := `{"type":"object","properties":{` +
		`"file_path":{"type":"string","description":"Path to read. Provide ` + "`file_path`" + ` before <content> & more."},` +
		`"limit":{"type":"number","description":"Maximum lines."},` +
		`"replace_all":{"type":"boolean"},` +
		`"todos":{"type":"array","description":"The complete list.","items":{"type":"object","additionalProperties":false,"properties":{` +
		`"content":{"type":"string","description":"What."},` +
		`"status":{"type":"string","description":"State.","enum":["pending","in_progress","completed"]}},"required":["content","status"]}},` +
		`"meta":{"type":"object","description":"Open metadata.","additionalProperties":true,"properties":{"note":{"type":"string"}}}},` +
		`"required":["file_path"]}`
	if got != want {
		t.Fatalf("schema\n got: %s\nwant: %s", got, want)
	}
	if !json.Valid([]byte(got)) {
		t.Fatal("schema is not JSON")
	}
	if empty := string(Parameters{}.marshal()); empty != `{"type":"object","properties":{}}` {
		t.Fatalf("empty schema = %s", empty)
	}
	if closed := string(Parameters{Optional("o", Object("", false))}.marshal()); closed != `{"type":"object","properties":{"o":{"type":"object","additionalProperties":false}}}` {
		t.Fatalf("memberless object = %s", closed)
	}
}

func TestParameters_ValidateAcceptsAndRejectsLikeUpstream(t *testing.T) {
	parameters := fullParameters()
	for _, raw := range []string{
		`{"file_path":"a"}`,
		`{"file_path":"a","limit":2.5,"replace_all":true,"todos":[{"content":"x","status":"pending"}],"meta":{"note":"n","extra":1}}`,
		`{"file_path":"a","limit":1e2}`,
	} {
		if err := parameters.validate(json.RawMessage(raw)); err != nil {
			t.Errorf("validate(%s) = %v", raw, err)
		}
	}
	for _, test := range []struct{ raw, want string }{
		{`[]`, `invalid arguments: "arguments" must be an object`},
		{`{}`, `invalid arguments: missing required property "file_path"`},
		{`{"file_path":null}`, `invalid arguments: "file_path" must be a string`},
		{`{"file_path":"a","limit":"2"}`, `invalid arguments: "limit" must be a number`},
		{`{"file_path":"a","limit":-0}`, `invalid arguments: "limit" must be a finite JSON number`},
		{`{"file_path":"a","limit":1e400}`, `invalid arguments: "limit" must be a finite JSON number`},
		{`{"file_path":"a","replace_all":"yes"}`, `invalid arguments: "replace_all" must be a boolean`},
		{`{"file_path":"a","todos":{}}`, `invalid arguments: "todos" must be an array`},
		{`{"file_path":"a","meta":[]}`, `invalid arguments: "meta" must be an object`},
		{`{"file_path":"a","todos":[{"content":"x","status":"done","why":1},{"status":"pending"}]}`,
			`invalid arguments: "todos[0].status" must be one of ["pending","in_progress","completed"]; "todos[0].why" is not a declared property; missing required property "todos[1].content"`},
		{`{"limt":1,"file_path":2,"other":true}`, `invalid arguments: "file_path" must be a string; "limt" is not a declared property; "other" is not a declared property`},
		{`{"file_path":"a","file_path":"b"}`, `invalid arguments: arguments repeat property "file_path"`},
		{`{"file_path":"a"} {}`, `invalid arguments: arguments contain a trailing value`},
		{`{"file_path":`, `invalid arguments: arguments are not valid JSON`},
		{`{"file_path":"a",}`, `invalid arguments: arguments are not valid JSON`},
		{`{"todos":[1,]}`, `invalid arguments: arguments are not valid JSON`},
		{strings.Repeat("[", maxArgumentDepth+2), `invalid arguments: arguments nest deeper than 64 levels`},
	} {
		err := parameters.validate(json.RawMessage(test.raw))
		if err == nil || !strings.HasPrefix(err.Error(), test.want) {
			t.Errorf("validate(%.40s) = %v, want %s", test.raw, err, test.want)
		}
	}
}

func TestDefine_RejectsInvalidSpecsAtRegistration(t *testing.T) {
	execute := func(context.Context, Invocation, fullArguments) (Result, error) { return Result{}, nil }
	valid := Spec[fullArguments]{Name: "valid", Description: "valid", Parameters: fullParameters(), Execute: execute}
	if tool := Define(valid); tool.err != nil {
		t.Fatalf("valid spec rejected: %v", tool.err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*Spec[fullArguments])
		want   string
	}{
		{"unnamed property", func(spec *Spec[fullArguments]) { spec.Parameters[0].Name = "" }, "unnamed property"},
		{"duplicate property", func(spec *Spec[fullArguments]) { spec.Parameters[1].Name = "file_path" }, `declares "file_path" twice`},
		{"enum on number", func(spec *Spec[fullArguments]) { spec.Parameters[1].Schema.Enum = []string{"1"} }, "enum on type"},
		{"array without items", func(spec *Spec[fullArguments]) { spec.Parameters[3].Schema.Items = nil }, "items exactly"},
		{"items on string", func(spec *Spec[fullArguments]) { spec.Parameters[0].Schema.Items = &Schema{Type: TypeString} }, "items exactly"},
		{"members on string", func(spec *Spec[fullArguments]) { spec.Parameters[0].Schema.AdditionalProperties = true }, "object members"},
		{"repeated enum", func(spec *Spec[fullArguments]) {
			spec.Parameters[3].Schema.Items.Properties[1].Schema.Enum = []string{"a", "a"}
		}, "repeats enum"},
		{"unknown type", func(spec *Spec[fullArguments]) { spec.Parameters[0].Schema.Type = "integer" }, "unsupported type"},
		{"bad item", func(spec *Spec[fullArguments]) { spec.Parameters[3].Schema.Items = &Schema{Type: "date"} }, "unsupported type"},
		{"bad name", func(spec *Spec[fullArguments]) { spec.Name = "has space" }, "tool name"},
		{"long name", func(spec *Spec[fullArguments]) { spec.Name = strings.Repeat("x", 65) }, "tool name"},
		{"empty description", func(spec *Spec[fullArguments]) { spec.Description = "" }, "description"},
		{"no execute", func(spec *Spec[fullArguments]) { spec.Execute = nil }, "execute is required"},
		{"missing field", func(spec *Spec[fullArguments]) {
			spec.Parameters = append(spec.Parameters, Optional("extra", String("")))
		}, "decodes 5 fields for 6"},
		{"renamed field", func(spec *Spec[fullArguments]) { spec.Parameters[0].Name = "path" }, `no field for "path"`},
		{"wrong kind", func(spec *Spec[fullArguments]) { spec.Parameters[1].Schema = String("") }, "decodes string into float64"},
		{"wrong item kind", func(spec *Spec[fullArguments]) {
			spec.Parameters[3].Schema.Items.Properties[0].Schema = Boolean("")
		}, "decodes boolean into string"},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := valid
			spec.Parameters = fullParameters()
			test.mutate(&spec)
			tool := Define(spec)
			if tool.err == nil || !strings.Contains(tool.err.Error(), test.want) {
				t.Fatalf("Define error = %v, want %q", tool.err, test.want)
			}
		})
	}
	type untagged struct{ Value string }
	if tool := Define(Spec[untagged]{Name: "untagged", Description: "x", Parameters: Parameters{Required("Value", String(""))}, Execute: func(context.Context, Invocation, untagged) (Result, error) { return Result{}, nil }}); tool.err == nil || !strings.Contains(tool.err.Error(), "JSON member name") {
		t.Fatalf("untagged error = %v", tool.err)
	}
	type hidden struct {
		Value string `json:"value"`
		state int
	}
	if tool := Define(Spec[hidden]{Name: "hidden", Description: "x", Parameters: Parameters{Required("value", String(""))}, Execute: func(context.Context, Invocation, hidden) (Result, error) { return Result{}, nil }}); tool.err != nil {
		t.Fatalf("unexported fields must be ignored: %v (%d)", tool.err, hidden{}.state)
	}
	if tool := Define(Spec[string]{Name: "scalar", Description: "x", Execute: func(context.Context, Invocation, string) (Result, error) { return Result{}, nil }}); tool.err == nil || !strings.Contains(tool.err.Error(), "struct") {
		t.Fatalf("scalar arguments error = %v", tool.err)
	}
}

func TestDefine_PreparesTypedArguments(t *testing.T) {
	var seen fullArguments
	tool := Define(Spec[fullArguments]{
		Name: "typed", Description: "typed", Parameters: fullParameters(),
		Execute: func(_ context.Context, _ Invocation, arguments fullArguments) (Result, error) {
			seen = arguments
			return Text("done"), nil
		},
	})
	prepared, err := tool.prepare(json.RawMessage(`{"file_path":"a","limit":3,"todos":[{"content":"c","status":"completed"}],"meta":{"note":"n","extra":1}}`))
	if err != nil || prepared.concurrent || prepared.check(context.Background(), Invocation{}) != nil || prepared.reason() != "" {
		t.Fatalf("prepared = %+v, %v", prepared, err)
	}
	if result, err := prepared.execute(context.Background(), Invocation{}); err != nil || result.Text != "done" {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if seen.FilePath != "a" || *seen.Limit != 3 || seen.All != nil || seen.Todos[0].Status != "completed" || seen.Meta.Note != "n" {
		t.Fatalf("arguments = %+v", seen)
	}
	definition := tool.Definition()
	if definition.Name != "typed" || definition.Description != "typed" || string(definition.Parameters) != string(fullParameters().marshal()) {
		t.Fatalf("definition = %#v", definition)
	}
	// encoding/json matches undeclared members of open objects
	// case-insensitively, so a schema-valid value can still fail to decode.
	if _, err := tool.prepare(json.RawMessage(`{"file_path":"a","meta":{"note":"n","NOTE":5}}`)); err == nil || !strings.HasPrefix(err.Error(), "invalid arguments: json") {
		t.Fatalf("decode error = %v", err)
	}
}

func TestStringEncoding_KeepsUpstreamCharacters(t *testing.T) {
	var buffer strings.Builder
	encoded := Parameters{Required("d", String("Use the language of the user’s current request → <mode> & done."))}.marshal()
	buffer.Write(encoded)
	if !strings.Contains(buffer.String(), "user’s current request → <mode> & done.") {
		t.Fatalf("escaped description: %s", buffer.String())
	}
}
