package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"reflect"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"gopkg.in/yaml.v3"
)

// v2.4.1 writes all six metadata fields, including a literal empty array after
// the final unbind. Do not infer zero from missing, null or duplicate fields.
func confirmedEmptyV241(data []byte) bool {
	if !json.Valid(data) {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	fields := make(map[string]any)
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return false
		}
		name := key.(string) // A valid JSON object has string keys.
		var value any
		if _, duplicate := fields[name]; duplicate || dec.Decode(&value) != nil {
			return false
		}
		fields[name] = value
	}
	rows, ok := fields["instances"].([]any)
	updated, _ := fields["updated_at"].(string)
	_, err := time.Parse(time.RFC3339, updated)
	return len(fields) == 6 && fields["config_mode"] == "instances" && fields["version"] == "v2.4.1" &&
		fields["instance_count"] == json.Number("0") && fields["latest_instance_id"] == "" && ok && len(rows) == 0 && err == nil
}

func explicitEmptyV241(data []byte) ([]byte, error) {
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	invalid := errors.New("configuration is not an unbound v2.4.1 layout")
	if dec.Decode(&doc) != nil || len(doc.Content) != 1 || dec.Decode(new(yaml.Node)) != io.EOF {
		return nil, invalid
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode || root.ShortTag() != "!!map" {
		return nil, invalid
	}
	// Only shared settings could remain in the old writer's fileRootConfig.
	var shared struct {
		Log     config.LogConfig
		Kernel  config.KernelConfig
		Node    config.NodeConfig
		WS      config.WSConfig
		Runtime config.RuntimeConfig
		Cert    config.CertConfig
	}
	dec = yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var original, typed map[string]map[string]any
	if dec.Decode(&shared) != nil || root.Decode(&original) != nil {
		return nil, invalid
	}
	// The YAML decoder can coerce e.g. an integer to a string. Require the
	// supplied fields to keep their values and types through typed decoding.
	normalized, err := yaml.Marshal(shared)
	if err != nil || yaml.Unmarshal(normalized, &typed) != nil {
		return nil, invalid
	}
	for section, fields := range original {
		if fields == nil {
			return nil, invalid
		}
		for key, value := range fields {
			if value == nil || !reflect.DeepEqual(value, typed[section][key]) {
				return nil, invalid
			}
		}
	}
	root.Style = 0 // The old systemd installer can leave the flow mapping {}.
	root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "instances"},
		&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle})
	return yaml.Marshal(&doc)
}

// Write only a new staged file. The owning installer validates it at the final
// deployment path and includes the original configuration in its rollback.
func runMigrateEmptyV241(args []string) error {
	fs := flag.NewFlagSet("migrate-empty-v241", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg := fs.String("config", "", "old configuration")
	meta := fs.String("meta", "", "old installation metadata")
	out := fs.String("output", "", "new staged file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfg == "" || *meta == "" || *out == "" || fs.NArg() != 0 {
		return errors.New("migrate-empty-v241 requires --config, --meta and --output")
	}
	m, err := os.ReadFile(*meta)
	if err != nil {
		return err
	}
	if !confirmedEmptyV241(m) {
		return errors.New("installation metadata does not confirm an empty v2.4.1 deployment")
	}
	data, err := os.ReadFile(*cfg)
	if err != nil {
		return err
	}
	data, err = explicitEmptyV241(data)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err = errors.Join(err, f.Close()); err != nil {
		return errors.Join(err, os.Remove(*out))
	}
	return nil
}
