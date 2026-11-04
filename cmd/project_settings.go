// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const (
	projSettingsReadScope  = "project:read"
	projSettingsWriteScope = "project:update"
	// projSettingsIdentityKey is the settings key for the default GCP
	// identity, handled by the 'scion project identity default' code.
	projSettingsIdentityKey = "defaultGCPIdentity"
)

var (
	projSettingsResolved bool
	projSettingsProfile  string
)

// resolveProjectForSettings returns the Hub client and the linked project.
// A variable so tests can point it at a fake Hub; it resolves the project
// the same way as 'scion project identity default'.
var resolveProjectForSettings = resolveLinkedProjectForSA

var projectSettingsCmd = &cobra.Command{
	Use:   "settings",
	Short: "View and change the project's settings on the Hub",
	Long: `View and change the project's settings on the Hub: the defaults the Hub
applies to agents created in this project (template, harness config and
auth, model, thinking level, active profile, limits, resources, agent
roles, telemetry, auto-expose ports, and the default GCP identity).

For agents created through the Hub, these settings decide the defaults: a
create with no --template gets the project's defaultTemplate, then the
Hub default, then the Hub's "default" template. The default_template and
active_profile keys in a local settings file ('scion config') are used by
the Runtime Broker only when none of these resolves (see ptone/scion#7080).

Keys are the API names (defaultTemplate), in any letter case, or the same
in snake_case or kebab-case (default_template, default-template). 'scion
project settings get' lists them. Keys below defaultResources and the
members of its object value are checked too.

The Hub has no compare-and-set for project settings. A change to part of
defaultResources reads the current value and saves it back whole, so a
change saved by someone else in between is lost (ptone/scion#7078).

Reading needs the project:read scope and writing project:update, on a
project or hub boundary, or a login with access to the project.`,
}

var projectSettingsGetCmd = &cobra.Command{
	Use:   "get [KEY]",
	Short: "Show the project settings, or one of them",
	Long: `Show the project's settings on the Hub, or one key.

--resolved also shows, for each setting, whether the project sets it and
whether the Hub has a default for it.

Examples:
  scion project settings get
  scion project settings get defaultTemplate
  scion project settings get --resolved`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := ""
		if len(args) == 1 {
			key = args[0]
		}
		client, projectID, err := resolveProjectForSettings()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		return runProjectSettingsGet(ctx, client.Projects(), projectID, cmd.OutOrStdout(), cmd.ErrOrStderr(), key, projSettingsResolved)
	},
}

var projectSettingsSetCmd = &cobra.Command{
	Use:   "set KEY VALUE [KEY VALUE ...]",
	Short: "Change project settings",
	Long: `Change one or more project settings in one save. Only the keys given
change.

Values are read by the setting's type: text, a whole number, or true/false.
For defaultMaxTurns and defaultMaxModelCalls, 0 or "unlimited" means no
limit (unset returns to the inherited default). defaultResources takes a
YAML or JSON object, or one of its keys (defaultResources.limits.cpu 2).

The default GCP identity is set with the key defaultGCPIdentity, as
'scion project identity default set' does: an account id, email or name,
or passthrough or block; --profile sets a broker profile's default account.

Examples:
  scion project settings set defaultTemplate claude
  scion project settings set defaultMaxTurns 200 telemetryEnabled false
  scion project settings set defaultResources.limits.memory 8Gi
  scion project settings set defaultGCPIdentity ci-runner@example.iam.gserviceaccount.com`,
	RunE: func(cmd *cobra.Command, args []string) error {
		changes, err := setArgsToChanges(args, "", true)
		if err != nil {
			return err
		}
		return runProjectSettingsCommand(cmd, changes)
	},
}

var projectSettingsUnsetCmd = &cobra.Command{
	Use:   "unset KEY [KEY ...]",
	Short: "Clear project settings, so the inherited default applies",
	Long: `Clear one or more project settings, so agents get the Hub's default (or
the built-in one) instead.

A key below defaultResources (defaultResources.limits.cpu) removes that part
and keeps the rest. defaultGCPIdentity clears the project default identity
(mode and account), or with --profile that profile's default account.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		changes := make([]settingsChange, 0, len(args))
		for _, k := range args {
			changes = append(changes, settingsChange{Key: k, Unset: true})
		}
		return runProjectSettingsCommand(cmd, changes)
	},
}

// runProjectSettingsCommand runs set or unset: the default GCP identity
// through the 'project identity default' code, everything else in one
// settings save.
func runProjectSettingsCommand(cmd *cobra.Command, changes []settingsChange) error {
	if err := checkProjectSettingsIdentityUse(changes, projSettingsProfile); err != nil {
		return err
	}
	if len(changes) == 1 && isProjectSettingsIdentityKey(changes[0].Key) {
		prev := identityDefaultProfile
		identityDefaultProfile = projSettingsProfile
		defer func() { identityDefaultProfile = prev }()
		if changes[0].Unset {
			return runIdentityDefaultUnset(cmd, nil)
		}
		return runIdentityDefaultSet(cmd, []string{fmt.Sprint(changes[0].Value)})
	}
	client, projectID, err := resolveProjectForSettings()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	return runProjectSettingsWrite(ctx, client.Projects(), projectID, cmd.OutOrStdout(), cmd.ErrOrStderr(), changes)
}

// checkProjectSettingsIdentityUse keeps the identity key on its own: it is
// saved by its own request, and --profile applies only to it.
func checkProjectSettingsIdentityUse(changes []settingsChange, profile string) error {
	identity := 0
	for _, ch := range changes {
		if isProjectSettingsIdentityKey(ch.Key) {
			if strings.Contains(ch.Key, ".") {
				return fmt.Errorf("%s has no keys below it (use --profile for a broker profile's account)", projSettingsIdentityKey)
			}
			identity++
		}
	}
	if identity > 0 && len(changes) > 1 {
		return fmt.Errorf("change %s in a command of its own", projSettingsIdentityKey)
	}
	if profile != "" && identity == 0 {
		return fmt.Errorf("--profile applies only to %s", projSettingsIdentityKey)
	}
	return nil
}

// --- the settings registry ------------------------------------------------

// projectSettingKind is how a setting's value is read on the command line.
type projectSettingKind int

const (
	kindText projectSettingKind = iota
	kindInt
	kindBool
	kindResources
	kindIdentity
)

// projectSettingClear is how 'unset' clears a setting: the Hub clears the
// tri-state fields on an explicit null, every text field on "" (a null
// there is ignored), and replaces defaultResources whole.
type projectSettingClear int

const (
	clearNull projectSettingClear = iota
	clearEmptyString
	clearEmptyObject
	clearByIdentity
)

// projectSettingsClearForms is the clear form of every project setting. A
// test pins that it covers every field of hubclient.ProjectSettings, so a
// field added there is classified here before it can be unset.
var projectSettingsClearForms = map[string]projectSettingClear{
	"activeProfile":          clearEmptyString,
	"defaultTemplate":        clearEmptyString,
	"defaultHarnessConfig":   clearEmptyString,
	"defaultHarnessAuth":     clearEmptyString,
	"defaultModel":           clearEmptyString,
	"defaultThinkingLevel":   clearNull,
	"telemetryEnabled":       clearNull,
	"autoExposePortsEnabled": clearNull,
	"defaultMaxTurns":        clearNull,
	"defaultMaxModelCalls":   clearNull,
	"defaultMaxDuration":     clearEmptyString,
	"defaultResources":       clearEmptyObject,
	"maxAgentRole":           clearEmptyString,
	"defaultAgentRole":       clearEmptyString,
	// The default GCP identity is changed through its own command code:
	// mode and account change together, and the per-profile map is
	// validated entry by entry.
	"defaultGCPIdentityMode":                      clearByIdentity,
	"defaultGCPIdentityServiceAccountID":          clearByIdentity,
	"defaultGCPIdentityServiceAccountIDByProfile": clearByIdentity,
}

// projectSettingsUnlimited lists the count limits where 0 means Unlimited.
var projectSettingsUnlimited = map[string]bool{"defaultMaxTurns": true, "defaultMaxModelCalls": true}

// projectSettingsNotStored are keys the Hub refuses at the project layer:
// they come from the server and Runtime Broker settings.
var projectSettingsNotStored = []string{"bucket", "runtimes", "harnesses", "profiles"}

// projectSettingField is one project setting.
type projectSettingField struct {
	Name string
	Kind projectSettingKind
}

// projectSettingFields lists the settings of hubclient.ProjectSettings by
// their JSON names, in declaration order.
func projectSettingFields() []projectSettingField {
	t := reflect.TypeOf(hubclient.ProjectSettings{})
	out := make([]projectSettingField, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out = append(out, projectSettingField{Name: name, Kind: projectSettingKindOf(name, f.Type)})
	}
	return out
}

func projectSettingKindOf(name string, t reflect.Type) projectSettingKind {
	if projectSettingsClearForms[name] == clearByIdentity {
		return kindIdentity
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int64:
		return kindInt
	case reflect.Bool:
		return kindBool
	case reflect.Struct:
		return kindResources
	default:
		return kindText
	}
}

// normalizeProjectSettingName folds a key for matching: the API name and
// its snake_case or kebab-case spelling match.
func normalizeProjectSettingName(s string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(s))
}

func isProjectSettingsIdentityKey(key string) bool {
	return normalizeProjectSettingName(strings.SplitN(key, ".", 2)[0]) == normalizeProjectSettingName(projSettingsIdentityKey)
}

// resolveProjectSettingKey returns the setting a key names and the path
// below it (for defaultResources), or an error that names the right key.
func resolveProjectSettingKey(key string) (projectSettingField, []string, error) {
	parts := strings.Split(strings.TrimSpace(key), ".")
	head := normalizeProjectSettingName(parts[0])
	for _, ns := range projectSettingsNotStored {
		if head == ns {
			return projectSettingField{}, nil, fmt.Errorf("%q is not a project setting: it comes from the server and Runtime Broker settings", key)
		}
	}
	for _, f := range projectSettingFields() {
		if normalizeProjectSettingName(f.Name) != head {
			continue
		}
		if f.Kind == kindIdentity {
			return projectSettingField{}, nil, fmt.Errorf("%q is part of the default GCP identity; use the key %s (with --profile for a broker profile's account)", key, projSettingsIdentityKey)
		}
		if len(parts) > 1 && f.Kind != kindResources {
			return projectSettingField{}, nil, fmt.Errorf("%q has no keys below it", f.Name)
		}
		return f, parts[1:], nil
	}
	msg := fmt.Sprintf("%q is not a project setting", key)
	if s := suggestProjectSetting(head); s != "" {
		msg += "; did you mean " + s + "?"
	}
	return projectSettingField{}, nil, fmt.Errorf("%s (see 'scion project settings get')", msg)
}

func suggestProjectSetting(norm string) string {
	best, bestD := "", suggestMaxDistance+1
	for _, f := range projectSettingFields() {
		n := normalizeProjectSettingName(f.Name)
		if !suggestInRange(norm, n) {
			continue
		}
		if d := levenshtein(norm, n); d < bestD {
			best, bestD = f.Name, d
		}
	}
	return best
}

// parseProjectSettingValue reads a command-line value by the setting's kind.
func parseProjectSettingValue(f projectSettingField, raw string) (interface{}, error) {
	switch f.Kind {
	case kindInt:
		if projectSettingsUnlimited[f.Name] && strings.EqualFold(raw, "unlimited") {
			return 0, nil
		}
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("%s takes a whole number", f.Name)
		}
		return n, nil
	case kindBool:
		b, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("%s takes true or false", f.Name)
		}
		return b, nil
	case kindResources:
		v, err := parseSettingsValue(raw, false)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
		m, ok := v.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("%s takes an object, for example {limits: {cpu: \"2\"}}, or one of its keys", f.Name)
		}
		stringifyResourceNumbers(m)
		if err := checkResourcesObject(m); err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
		return m, nil
	default:
		if raw == "" {
			return nil, fmt.Errorf("%s: an empty value; use 'unset' to clear it", f.Name)
		}
		return raw, nil
	}
}

// buildProjectSettingsBody turns changes into a sparse settings body. A
// change below defaultResources is applied to the current value (current,
// from GET) and the whole value is sent.
func buildProjectSettingsBody(current *hubclient.ProjectSettings, changes []settingsChange) (map[string]interface{}, error) {
	if len(changes) == 0 {
		return nil, fmt.Errorf("no project settings to change")
	}
	body := map[string]interface{}{}
	below := map[string]map[string]interface{}{} // field -> working copy
	whole := map[string]bool{}
	for _, ch := range changes {
		if isProjectSettingsIdentityKey(ch.Key) {
			return nil, fmt.Errorf("change %s in a command of its own", projSettingsIdentityKey)
		}
		f, sub, err := resolveProjectSettingKey(ch.Key)
		if err != nil {
			return nil, err
		}
		if len(sub) == 0 {
			if _, dup := body[f.Name]; dup || below[f.Name] != nil {
				return nil, fmt.Errorf("%s is given more than once", f.Name)
			}
			whole[f.Name] = true
			if ch.Unset {
				body[f.Name] = projectSettingClearValue(f.Name)
				continue
			}
			v, err := parseProjectSettingValue(f, fmt.Sprint(ch.Value))
			if err != nil {
				return nil, err
			}
			body[f.Name] = v
			continue
		}
		if whole[f.Name] {
			return nil, fmt.Errorf("%s is changed both as a whole and below it", f.Name)
		}
		sub, err = checkResourcesPath(sub, !ch.Unset)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", ch.Key, err)
		}
		work, ok := below[f.Name]
		if !ok {
			work, err = currentProjectSettingObject(current, f.Name)
			if err != nil {
				return nil, err
			}
			below[f.Name] = work
		}
		if ch.Unset {
			if err := deleteAtPath(work, sub); err != nil {
				return nil, fmt.Errorf("cannot unset %q: %w", ch.Key, err)
			}
			pruneEmptyObjects(work)
			continue
		}
		if err := putAtPath(work, sub, fmt.Sprint(ch.Value)); err != nil {
			return nil, fmt.Errorf("cannot set %q: %w", ch.Key, err)
		}
	}
	for name, work := range below {
		body[name] = work
	}
	return body, nil
}

// projectSettingClearValue is the value 'unset' sends for a setting.
func projectSettingClearValue(name string) interface{} {
	switch projectSettingsClearForms[name] {
	case clearEmptyString:
		return ""
	case clearEmptyObject:
		return map[string]interface{}{}
	default:
		return nil
	}
}

// currentProjectSettingObject returns a field's current value as a JSON
// object (an empty one when it is not set).
func currentProjectSettingObject(current *hubclient.ProjectSettings, name string) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	if current == nil {
		return out, nil
	}
	b, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	if raw, ok := all[name]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("read current %s: %w", name, err)
		}
	}
	return out, nil
}

// resourcesKeys returns the JSON member names of a struct type, keyed by
// name, with the type of each member (nil for a leaf).
func resourcesKeys(t reflect.Type) map[string]reflect.Type {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			out[name] = ft
		} else {
			out[name] = nil
		}
	}
	return out
}

// checkResourcesPath checks a path below defaultResources against
// hubclient.ProjectResourceSpec and returns it with each segment in its
// canonical spelling. Segments match in any letter case, as the Hub's JSON
// decoding does. Every segment must be a member, and a set must end at a
// leaf (an unset may remove a whole member). The Hub drops unknown members
// silently, so a typo is refused here.
func checkResourcesPath(sub []string, leaf bool) ([]string, error) {
	t := reflect.TypeOf(hubclient.ProjectResourceSpec{})
	canon := make([]string, 0, len(sub))
	for i, seg := range sub {
		keys := resourcesKeys(t)
		next, ok := keys[seg]
		if !ok {
			for k, v := range keys {
				if strings.EqualFold(k, seg) {
					seg, next, ok = k, v, true
					break
				}
			}
		}
		if !ok {
			names := make([]string, 0, len(keys))
			for k := range keys {
				names = append(names, k)
			}
			sort.Strings(names)
			msg := fmt.Sprintf("%q is not a key of defaultResources%s", seg, resourcesPathLabel(sub[:i]))
			if best := closestName(seg, names); best != "" {
				msg += "; did you mean " + best + "?"
			}
			return nil, fmt.Errorf("%s (keys: %s)", msg, strings.Join(names, ", "))
		}
		canon = append(canon, seg)
		if next == nil {
			if i != len(sub)-1 {
				return nil, fmt.Errorf("%s has no keys below it", strings.Join(canon, "."))
			}
			return canon, nil
		}
		t = next
	}
	if leaf {
		return nil, fmt.Errorf("defaultResources%s is an object: set one of its keys, or set defaultResources to a whole object", resourcesPathLabel(canon))
	}
	return canon, nil
}

func resourcesPathLabel(sub []string) string {
	if len(sub) == 0 {
		return ""
	}
	return "." + strings.Join(sub, ".")
}

func closestName(s string, names []string) string {
	best, bestD := "", suggestMaxDistance+1
	for _, n := range names {
		if !suggestInRange(s, n) {
			continue
		}
		if d := levenshtein(s, n); d < bestD {
			best, bestD = n, d
		}
	}
	return best
}

// checkResourcesObject decodes an object value for defaultResources
// strictly, so an unknown member (which the Hub would drop) is refused.
func checkResourcesObject(m map[string]interface{}) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var spec hubclient.ProjectResourceSpec
	if err := dec.Decode(&spec); err != nil {
		return fmt.Errorf("%v (keys: requests, limits, disk; requests and limits take cpu and memory)", err)
	}
	return nil
}

// stringifyResourceNumbers turns number leaves of a defaultResources object
// into strings: the quantities are text in the API ("2", "8Gi"), and YAML
// reads an unquoted 2 as a number.
func stringifyResourceNumbers(m map[string]interface{}) {
	for k, v := range m {
		switch t := v.(type) {
		case map[string]interface{}:
			stringifyResourceNumbers(t)
		case int, int64, float64:
			m[k] = fmt.Sprint(t)
		}
	}
}

// pruneEmptyObjects removes members left as empty objects after an unset.
func pruneEmptyObjects(m map[string]interface{}) {
	for k, v := range m {
		if sub, ok := v.(map[string]interface{}); ok {
			pruneEmptyObjects(sub)
			if len(sub) == 0 {
				delete(m, k)
			}
		}
	}
}

// --- run functions ----------------------------------------------------------

func runProjectSettingsGet(ctx context.Context, svc hubclient.ProjectService, projectID string, out, errw io.Writer, key string, resolved bool) error {
	format, err := settingsOutputFormat()
	if err != nil {
		return err
	}
	if resolved {
		if key != "" {
			return fmt.Errorf("use either KEY or --resolved, not both")
		}
		if format != "" {
			body, err := svc.GetResolvedSettingsBody(ctx, projectID)
			if err != nil {
				return reportSettingsError(out, errw, format, err, settingsScopeHint{Scope: projSettingsReadScope})
			}
			return writeHubBody(out, format, body)
		}
		res, err := svc.GetResolvedSettings(ctx, projectID)
		if err != nil {
			return reportSettingsError(out, errw, format, err, settingsScopeHint{Scope: projSettingsReadScope})
		}
		printResolvedProjectSettings(out, res)
		return nil
	}
	if key == "" && format != "" {
		body, err := svc.GetSettingsBody(ctx, projectID)
		if err != nil {
			return reportSettingsError(out, errw, format, err, settingsScopeHint{Scope: projSettingsReadScope})
		}
		return writeHubBody(out, format, body)
	}
	cur, err := svc.GetSettings(ctx, projectID)
	if err != nil {
		return reportSettingsError(out, errw, format, err, settingsScopeHint{Scope: projSettingsReadScope})
	}
	values, err := projectSettingsValues(cur)
	if err != nil {
		return err
	}
	if key != "" {
		name := projSettingsIdentityKey
		if isProjectSettingsIdentityKey(key) && strings.Contains(key, ".") {
			return fmt.Errorf("%s has no keys below it", projSettingsIdentityKey)
		}
		if !isProjectSettingsIdentityKey(key) {
			f, sub, err := resolveProjectSettingKey(key)
			if err != nil {
				return err
			}
			if len(sub) > 0 {
				sub, err = checkResourcesPath(sub, false)
				if err != nil {
					return fmt.Errorf("%q: %w", key, err)
				}
				obj, err := currentProjectSettingObject(cur, f.Name)
				if err != nil {
					return err
				}
				raw, ok := hubclient.LookupDotted(rawObject(obj), strings.Join(sub, "."))
				if !ok {
					return fmt.Errorf("%q is not set", key)
				}
				return printProjectSettingValue(out, format, key, raw)
			}
			name = f.Name
		}
		if name == projSettingsIdentityKey {
			return writeStructuredOrText(out, format, identityDefaultsView(cur))
		}
		raw, ok := values[name]
		if !ok {
			return fmt.Errorf("%s is not set", name)
		}
		return printProjectSettingValue(out, format, name, raw)
	}
	printProjectSettings(out, values)
	return nil
}

// writeHubBody prints a Hub response body as received: JSON is only
// re-indented, and YAML keeps the members, their order and their values.
// Nothing is decoded into a typed value first, so no member is added,
// dropped or changed (a masked value stays masked).
func writeHubBody(out io.Writer, format string, body json.RawMessage) error {
	if format == "yaml" {
		var doc yaml.Node
		if err := yaml.Unmarshal(body, &doc); err != nil {
			return fmt.Errorf("reading the Hub response: %w", err)
		}
		clearYAMLStyle(&doc)
		enc := yaml.NewEncoder(out)
		enc.SetIndent(2)
		if err := enc.Encode(&doc); err != nil {
			return err
		}
		return enc.Close()
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, body, "", "  "); err != nil {
		return fmt.Errorf("reading the Hub response: %w", err)
	}
	buf.WriteByte('\n')
	_, err := out.Write(buf.Bytes())
	return err
}

// clearYAMLStyle drops the JSON flow and quoting styles so the document
// prints in block style; the encoder still quotes a string that would
// otherwise read as another type.
func clearYAMLStyle(n *yaml.Node) {
	n.Style = 0
	for _, c := range n.Content {
		clearYAMLStyle(c)
	}
}

func runProjectSettingsWrite(ctx context.Context, svc hubclient.ProjectService, projectID string, out, errw io.Writer, changes []settingsChange) error {
	format, err := settingsOutputFormat()
	if err != nil {
		return err
	}
	// Resolve every key before any request: an unknown or not-stored key
	// sends nothing.
	needCurrent := false
	for _, ch := range changes {
		f, sub, err := resolveProjectSettingKey(ch.Key)
		if err != nil {
			return err
		}
		if len(sub) > 0 {
			if _, err := checkResourcesPath(sub, !ch.Unset); err != nil {
				return fmt.Errorf("%q: %w", ch.Key, err)
			}
			needCurrent = true
		} else if !ch.Unset && f.Kind == kindResources {
			if _, err := parseProjectSettingValue(f, fmt.Sprint(ch.Value)); err != nil {
				return err
			}
		}
	}
	var cur *hubclient.ProjectSettings
	if needCurrent {
		if cur, err = svc.GetSettings(ctx, projectID); err != nil {
			return reportSettingsError(out, errw, format, err, settingsScopeHint{Scope: projSettingsReadScope})
		}
	}
	body, err := buildProjectSettingsBody(cur, changes)
	if err != nil {
		return err
	}
	res, err := svc.UpdateSettingsFields(ctx, projectID, body)
	if err != nil {
		return reportSettingsError(out, errw, format, err, settingsScopeHint{Scope: projSettingsWriteScope})
	}
	if format != "" {
		return writeStructured(out, format, res)
	}
	values, err := projectSettingsValues(&res.ProjectSettings)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "Saved. Project settings now:")
	printProjectSettings(out, values)
	for _, w := range res.Warnings {
		_, _ = fmt.Fprintf(errw, "Warning: %s\n", w)
	}
	return nil
}

// --- output -----------------------------------------------------------------

// projectSettingsValues returns the set fields of s as JSON values, keyed
// by API name.
func projectSettingsValues(s *hubclient.ProjectSettings) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if s == nil {
		return out, nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	for k, v := range out {
		if string(v) == "null" || string(v) == "{}" {
			delete(out, k)
		}
	}
	return out, nil
}

func printProjectSettings(out io.Writer, values map[string]json.RawMessage) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SETTING\tVALUE")
	for _, f := range projectSettingFields() {
		raw, ok := values[f.Name]
		text := "-"
		if ok {
			if t, err := formatSettingsValue(raw); err == nil {
				text = strings.ReplaceAll(t, "\n", " ")
			}
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", f.Name, text)
	}
	_ = tw.Flush()
}

func printResolvedProjectSettings(out io.Writer, res *hubclient.ResolvedProjectSettings) {
	keys := make([]string, 0, len(res.Settings))
	for k := range res.Settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SETTING\tPROJECT VALUE\tHUB DEFAULT")
	for _, k := range keys {
		s := res.Settings[k]
		pv := "-"
		if s.ProjectSet && s.ProjectValue != nil {
			pv = *s.ProjectValue
			if pv == "" {
				pv = `""`
			}
		}
		// Tri-state as the Hub reports it: present, absent or unknown
		// (unknown is not "no").
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", k, pv, orDash(string(s.HubDefault)))
	}
	_ = tw.Flush()
}

func printProjectSettingValue(out io.Writer, format, key string, raw json.RawMessage) error {
	if format != "" {
		return writeStructured(out, format, map[string]json.RawMessage{key: raw})
	}
	text, err := formatSettingsValue(raw)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, text)
	return nil
}

func writeStructuredOrText(out io.Writer, format string, v interface{}) error {
	if format == "" {
		format = "yaml"
	}
	return writeStructured(out, format, v)
}

func identityDefaultsView(s *hubclient.ProjectSettings) map[string]interface{} {
	byProfile := s.DefaultGCPIdentityServiceAccountIDByProfile
	if byProfile == nil {
		byProfile = map[string]string{}
	}
	return map[string]interface{}{
		"defaultGCPIdentityMode":                      s.DefaultGCPIdentityMode,
		"defaultGCPIdentityServiceAccountID":          s.DefaultGCPIdentityServiceAccountID,
		"defaultGCPIdentityServiceAccountIDByProfile": byProfile,
	}
}

func rawObject(m map[string]interface{}) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		if b, err := json.Marshal(v); err == nil {
			out[k] = b
		}
	}
	return out
}

func init() {
	projectCmd.AddCommand(projectSettingsCmd)
	projectSettingsCmd.AddCommand(projectSettingsGetCmd, projectSettingsSetCmd, projectSettingsUnsetCmd)
	projectSettingsGetCmd.Flags().BoolVar(&projSettingsResolved, "resolved", false, "Also show whether the project sets each setting and whether the Hub has a default for it")
	for _, c := range []*cobra.Command{projectSettingsSetCmd, projectSettingsUnsetCmd} {
		c.Flags().StringVar(&projSettingsProfile, "profile", "", "With defaultGCPIdentity: the broker profile whose default account to change")
	}
}
