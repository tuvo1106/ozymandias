package collector

import (
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Int is a whole-number check setting that accepts a YAML number or a
// numeric string. Both reach a check: a config file writes `port: 6379`, but
// an autodiscovery label's value is text, and a user quoting it in YAML
// (`port: "6379"`) means the same thing. A plain int field would refuse the
// string; a string field would push the parsing into every check.
type Int int

// UnmarshalYAML implements yaml.Unmarshaler.
func (n *Int) UnmarshalYAML(v *yaml.Node) error {
	if v.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: want a whole number", v.Line)
	}
	i, err := strconv.Atoi(v.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a whole number", v.Line, v.Value)
	}
	*n = Int(i)
	return nil
}

// Port is a TCP port setting: an [Int] from 1 to 65535. Checked while
// decoding, so every check refuses port 0 or 70000 the same way. The zero
// value means "not set": a field left out is never decoded, and the check
// supplies its default.
type Port int

// UnmarshalYAML implements yaml.Unmarshaler.
func (p *Port) UnmarshalYAML(v *yaml.Node) error {
	var n Int
	if err := n.UnmarshalYAML(v); err != nil {
		return err // already names its line, which Instance.Decode turns into the setting
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("line %d: port %d: want a number from 1 to 65535", v.Line, n)
	}
	*p = Port(n)
	return nil
}
