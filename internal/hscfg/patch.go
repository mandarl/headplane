package hscfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Patch is one dotted-path config mutation. A nil Value deletes the key.
type Patch struct {
	Path  string
	Value any // nil deletes the key
}

// SplitPatchPath splits a dotted patch path honoring double-quoted segments,
// e.g. `dns.nameservers.split."foo.bar"` -> ["dns","nameservers","split",
// "foo.bar"]. It ports the char-scan algorithm from config-loader.ts: a dot
// splits unless inside quotes. Unlike the TS code (which only strips quotes
// from the final segment — a latent bug), quotes are stripped from every
// segment; this is identical for every real path since quoted segments are
// always last.
func SplitPatchPath(path string) []string {
	var segs []string
	var current strings.Builder
	quote := false
	for _, char := range path {
		if char == '"' {
			quote = !quote
		}
		if char == '.' && !quote {
			segs = append(segs, stripQuotes(current.String()))
			current.Reset()
			continue
		}
		current.WriteRune(char)
	}
	segs = append(segs, stripQuotes(current.String()))
	return segs
}

func stripQuotes(s string) string {
	return strings.ReplaceAll(s, `"`, "")
}

// Patch mirrors HeadscaleConfig.patch: apply setIn/deleteIn mutations to the
// retained YAML document (comments and formatting preserved via yaml.Node),
// revalidate with strictlyValid, and only then write the file back
// (preserving the file's mode) and refresh the in-memory extracted view.
// If revalidation fails, neither the file nor memory is changed and an error
// is returned. Patch requires Writable() and serializes concurrent callers
// with the config mutex.
func (c *Config) Patch(patches []Patch) error {
	if c == nil {
		return errors.New("hscfg: no Headscale config loaded")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.patchLocked(patches)
}

func (c *Config) patchLocked(patches []Patch) error {
	if c.path == "" || c.doc == nil || !c.Writable() {
		return errors.New("hscfg: Headscale config is not writable")
	}

	// Work on a clone so a failed revalidation leaves the live document,
	// the file, and the extracted view untouched (the TS port mutates its
	// document in place even when validation fails; we don't).
	clone := cloneNode(c.doc)
	for _, p := range patches {
		segs := SplitPatchPath(p.Path)
		if p.Value == nil {
			deleteIn(clone, segs)
			continue
		}
		vn, err := valueNode(p.Value)
		if err != nil {
			return fmt.Errorf("hscfg: cannot encode patch value for %q: %w", p.Path, err)
		}
		setIn(clone, segs, vn)
	}

	var raw map[string]any
	if err := clone.Decode(&raw); err != nil {
		return fmt.Errorf("hscfg: patched config does not decode: %w", err)
	}
	if !strictlyValid(raw) {
		return errors.New("hscfg: patch would leave the Headscale config invalid; no changes written")
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2) // match the 2-space style of typical headscale configs
	if err := enc.Encode(clone); err != nil {
		_ = enc.Close()
		return fmt.Errorf("hscfg: cannot serialize patched config: %w", err)
	}
	_ = enc.Close()

	mode := os.FileMode(0o644)
	if info, err := os.Stat(c.path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(c.path, buf.Bytes(), mode); err != nil {
		return fmt.Errorf("hscfg: cannot write Headscale config: %w", err)
	}

	c.doc = clone
	c.extract(raw)
	return nil
}

// AddDNSRecord mirrors HeadscaleConfig.addDNS. When a separate DNS records
// file is configured (file mode), the file must be rw or this is a no-op
// returning (false, nil); a record with the same name+type is skipped;
// otherwise the record is appended and the file rewritten, returning
// (false, nil) — file mode never needs an integration restart. Without a
// DNS file (config mode), the record goes through Patch into
// dns.extra_records and returns (true, nil) when it changed, (false, nil)
// when the record already existed.
func (c *Config) AddDNSRecord(rec DNSRecord) (restart bool, err error) {
	if c == nil {
		return false, errors.New("hscfg: no Headscale config loaded")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.dnsPath != "" {
		if c.dnsAccess != AccessRW {
			return false, nil // not writable: TS logs and returns undefined
		}
		for _, r := range c.FileRecords {
			if r.Name == rec.Name && r.Type == rec.Type {
				return false, nil
			}
		}
		if err := c.writeDNSFile(append(append([]DNSRecord{}, c.FileRecords...), rec)); err != nil {
			return false, err
		}
		return false, nil
	}

	for _, r := range c.ExtraRecords {
		if r.Name == rec.Name && r.Type == rec.Type {
			return false, nil
		}
	}
	updated := append(append([]DNSRecord{}, c.ExtraRecords...), rec)
	if err := c.patchLocked([]Patch{{Path: "dns.extra_records", Value: dnsRecordsValue(updated)}}); err != nil {
		return false, err
	}
	return true, nil
}

// RemoveDNSRecord mirrors HeadscaleConfig.removeDNS. In file mode it filters
// name+type out of the DNS records file and returns (false, nil). In config
// mode it patches dns.extra_records, returning (true, nil) only when a
// record was actually removed and (false, nil) when nothing changed.
func (c *Config) RemoveDNSRecord(name, typ string) (restart bool, err error) {
	if c == nil {
		return false, errors.New("hscfg: no Headscale config loaded")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	keep := func(r DNSRecord) bool { return r.Name != name || r.Type != typ }

	if c.dnsPath != "" {
		if c.dnsAccess != AccessRW {
			return false, nil // not writable: TS logs and returns undefined
		}
		filtered := make([]DNSRecord, 0, len(c.FileRecords))
		for _, r := range c.FileRecords {
			if keep(r) {
				filtered = append(filtered, r)
			}
		}
		// TS writes unconditionally here, even when nothing changed.
		if err := c.writeDNSFile(filtered); err != nil {
			return false, err
		}
		return false, nil
	}

	filtered := make([]DNSRecord, 0, len(c.ExtraRecords))
	for _, r := range c.ExtraRecords {
		if keep(r) {
			filtered = append(filtered, r)
		}
	}
	if len(filtered) == len(c.ExtraRecords) {
		return false, nil
	}
	if err := c.patchLocked([]Patch{{Path: "dns.extra_records", Value: dnsRecordsValue(filtered)}}); err != nil {
		return false, err
	}
	return true, nil
}

// writeDNSFile mirrors HeadscaleDNSConfig.write: JSON.stringify(records,
// null, 4) with no trailing newline, preserving the file's mode, and
// refreshes the in-memory view. Callers hold c.mu.
func (c *Config) writeDNSFile(records []DNSRecord) error {
	data, err := json.MarshalIndent(records, "", "    ")
	if err != nil {
		return fmt.Errorf("hscfg: cannot encode DNS records: %w", err)
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(c.dnsPath); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(c.dnsPath, data, mode); err != nil {
		return fmt.Errorf("hscfg: cannot write DNS records file: %w", err)
	}
	c.FileRecords = records
	return nil
}

// dnsRecordsValue converts records to the []any of map[string]any that the
// dns.extra_records patch value carries.
func dnsRecordsValue(recs []DNSRecord) []any {
	out := make([]any, 0, len(recs))
	for _, r := range recs {
		out = append(out, map[string]any{"name": r.Name, "type": r.Type, "value": r.Value})
	}
	return out
}

// cloneNode deep-copies a yaml.Node so patches can be validated before
// committing them to the live document.
func cloneNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	out := *n
	out.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		out.Content[i] = cloneNode(child)
	}
	return &out
}

// valueNode converts a Go patch value (string, bool, []string, []any,
// map[string]any, DNSRecord slices, …) into a yaml.Node via a YAML
// round-trip: marshal the Go value, then decode it into a Node. This keeps
// sequences/mappings/scalars correct without hand-rolling per-type logic.
func valueNode(v any) (*yaml.Node, error) {
	data, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	var n yaml.Node
	if err := yaml.Unmarshal(data, &n); err != nil {
		return nil, err
	}
	if len(n.Content) == 0 {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}, nil
	}
	return n.Content[0], nil
}

// docRoot unwraps a DocumentNode to its content mapping.
func docRoot(doc *yaml.Node) *yaml.Node {
	if doc == nil {
		return nil
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return doc
}

// ensureMapping returns the document's root mapping, creating it for an
// empty document.
func ensureMapping(doc *yaml.Node) *yaml.Node {
	if root := docRoot(doc); root != nil {
		return root
	}
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if doc.Kind == yaml.DocumentNode {
		doc.Content = []*yaml.Node{m}
	}
	return m
}

// mappingValue returns the value node for key in a mapping node, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if k := m.Content[i]; k.Kind == yaml.ScalarNode && k.Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setKey sets key to val in mapping m, appending the pair when absent.
func setKey(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if k := m.Content[i]; k.Kind == yaml.ScalarNode && k.Value == key {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
}

// setIn navigates/creates nested mappings along segs and sets the leaf,
// mirroring yaml Document.setIn.
func setIn(doc *yaml.Node, segs []string, val *yaml.Node) {
	if len(segs) == 0 {
		return
	}
	cur := ensureMapping(doc)
	for _, s := range segs[:len(segs)-1] {
		next := mappingValue(cur, s)
		if next == nil || next.Kind != yaml.MappingNode {
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setKey(cur, s, next)
		}
		cur = next
	}
	setKey(cur, segs[len(segs)-1], val)
}

// deleteIn removes the leaf key from its parent mapping, mirroring yaml
// Document.deleteIn. Missing intermediate keys are a no-op.
func deleteIn(doc *yaml.Node, segs []string) {
	if len(segs) == 0 {
		return
	}
	root := docRoot(doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return
	}
	cur := root
	for _, s := range segs[:len(segs)-1] {
		next := mappingValue(cur, s)
		if next == nil || next.Kind != yaml.MappingNode {
			return
		}
		cur = next
	}
	last := segs[len(segs)-1]
	for i := 0; i+1 < len(cur.Content); i += 2 {
		if k := cur.Content[i]; k.Kind == yaml.ScalarNode && k.Value == last {
			cur.Content = append(cur.Content[:i], cur.Content[i+2:]...)
			return
		}
	}
}
