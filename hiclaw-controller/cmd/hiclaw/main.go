package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// Backend abstraction: embedded mode uses MinIO (mc), incluster mode uses client-go
var kubeMode string

func init() {
	kubeMode = os.Getenv("HICLAW_KUBE_MODE")
	if kubeMode == "" {
		kubeMode = "embedded"
	}
}

func main() {
	rootCmd := &cobra.Command{
		Use:   "hiclaw",
		Short: "HiClaw declarative resource management CLI",
	}

	rootCmd.AddCommand(applyCmd())
	rootCmd.AddCommand(getCmd())
	rootCmd.AddCommand(deleteCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// --- MinIO helpers (embedded mode) ---

func mcExec(args ...string) (string, error) {
	cmd := exec.Command("mc", args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func storagePrefix() string {
	prefix := os.Getenv("HICLAW_STORAGE_PREFIX")
	if prefix == "" {
		prefix = "hiclaw/hiclaw-storage"
	}
	return prefix
}

func configPath(kind, name string) string {
	return fmt.Sprintf("%s/hiclaw-config/%ss/%s.yaml", storagePrefix(), kind, name)
}

func configDir(kind string) string {
	return fmt.Sprintf("%s/hiclaw-config/%ss/", storagePrefix(), kind)
}

// --- apply ---

func applyCmd() *cobra.Command {
	var files []string
	var zipFile string
	var name string
	var prune bool
	var dryRun bool
	var yes bool

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply resource configuration from YAML files or ZIP packages",
		Long: `Apply creates or updates resources defined in YAML files.
Use --zip with --name to import a legacy Worker/Team package (manifest.json).
Use --prune to also delete resources not present in the YAML (full sync).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Handle --zip: convert manifest.json to YAML, upload ZIP + YAML to MinIO
			if zipFile != "" {
				if name == "" {
					return fmt.Errorf("--name is required when using --zip (uniquely identifies the worker)")
				}
				return applyZip(zipFile, name, dryRun)
			}

			if len(files) == 0 {
				return fmt.Errorf("at least one -f/--file or --zip is required")
			}

			resources, err := loadResources(files)
			if err != nil {
				return err
			}

			if dryRun {
				fmt.Println("Dry-run mode: showing planned changes")
				fmt.Println()
			}

			if kubeMode == "incluster" {
				return applyInCluster(resources, prune, dryRun)
			}
			return applyEmbedded(resources, prune, dryRun, yes)
		},
	}

	cmd.Flags().StringArrayVarP(&files, "file", "f", nil, "YAML resource file(s)")
	cmd.Flags().StringVar(&zipFile, "zip", "", "Legacy ZIP package (manifest.json)")
	cmd.Flags().StringVar(&name, "name", "", "Resource name (required with --zip)")
	cmd.Flags().BoolVar(&prune, "prune", false, "Delete resources not in YAML")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show changes without applying")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip delete confirmation")

	return cmd
}

// applyEmbedded writes YAML files to MinIO hiclaw-config/{kind}s/{name}.yaml
func applyEmbedded(resources []resource, prune, dryRun, yes bool) error {
	applied := map[string]map[string]bool{
		"worker": {},
		"team":   {},
		"human":  {},
	}

	// Apply order: Team → Worker → Human
	ordered := orderForApply(resources)

	for _, r := range ordered {
		kind := strings.ToLower(r.Kind)
		dest := configPath(kind, r.Name)

		// Check if resource already exists
		_, existErr := mcExec("stat", dest)
		action := "created"
		if existErr == nil {
			action = "configured"
		}

		if dryRun {
			fmt.Printf("  %s/%s → %s (%s, dry-run)\n", r.Kind, r.Name, dest, action)
		} else {
			// Write YAML to temp file, then mc cp to MinIO
			tmpFile, err := writeTempYAML(r.Raw)
			if err != nil {
				return fmt.Errorf("failed to write temp file for %s/%s: %w", r.Kind, r.Name, err)
			}
			defer os.Remove(tmpFile)

			if _, err := mcExec("cp", tmpFile, dest); err != nil {
				return fmt.Errorf("failed to upload %s/%s to MinIO: %w", r.Kind, r.Name, err)
			}
			fmt.Printf("  %s/%s %s\n", r.Kind, r.Name, action)
		}

		if applied[kind] != nil {
			applied[kind][r.Name] = true
		}
	}

	// Prune: delete MinIO files not in YAML
	if prune {
		deleted := 0
		for _, kind := range []string{"human", "worker", "team"} { // delete order: Human → Worker → Team
			existing, err := listMinIOResources(kind)
			if err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: failed to list %ss from MinIO: %v\n", kind, err)
				continue
			}
			for _, name := range existing {
				if !applied[kind][name] {
					path := configPath(kind, name)
					if dryRun {
						fmt.Printf("  %s/%s would be deleted (dry-run)\n", kind, name)
						deleted++
					} else {
						if !yes {
							fmt.Printf("  Delete %s/%s? [y/N] ", kind, name)
							reader := bufio.NewReader(os.Stdin)
							answer, _ := reader.ReadString('\n')
							if strings.TrimSpace(strings.ToLower(answer)) != "y" {
								continue
							}
						}
						if _, err := mcExec("rm", path); err != nil {
							fmt.Fprintf(os.Stderr, "WARNING: failed to delete %s: %v\n", path, err)
						} else {
							fmt.Printf("  %s/%s deleted\n", kind, name)
							deleted++
						}
					}
				}
			}
		}
		if deleted > 0 {
			fmt.Printf("\n%d resource(s) pruned\n", deleted)
		}
	}

	return nil
}

// applyInCluster uses client-go to apply resources to K8s API Server
func applyInCluster(resources []resource, prune, dryRun bool) error {
	// TODO: implement client-go apply for incluster mode
	fmt.Println("incluster mode: not yet implemented")
	return nil
}

// listMinIOResources lists resource names from MinIO hiclaw-config/{kind}s/
func listMinIOResources(kind string) ([]string, error) {
	dir := configDir(kind)
	out, err := mcExec("ls", "--json", dir)
	if err != nil {
		return nil, err
	}

	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// mc ls --json outputs {"key":"name.yaml",...}
		// Simple extraction without json dependency
		if idx := strings.Index(line, `"key":"`); idx >= 0 {
			rest := line[idx+7:]
			if end := strings.Index(rest, `"`); end >= 0 {
				filename := rest[:end]
				if strings.HasSuffix(filename, ".yaml") || strings.HasSuffix(filename, ".yml") {
					name := strings.TrimSuffix(strings.TrimSuffix(filename, ".yaml"), ".yml")
					if name != ".gitkeep" && name != "" {
						names = append(names, name)
					}
				}
			}
		}
	}
	return names, nil
}

// --- get ---

func getCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <resource-type> [name]",
		Short: "Display resources",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			resourceType := args[0]
			name := ""
			if len(args) > 1 {
				name = args[1]
			}

			kind := strings.TrimSuffix(resourceType, "s")
			switch kind {
			case "worker", "team", "human":
			default:
				return fmt.Errorf("unknown resource type %q (use: workers, teams, humans)", resourceType)
			}

			if kubeMode == "incluster" {
				// TODO: client-go list/get
				fmt.Println("incluster mode: not yet implemented")
				return nil
			}

			if name != "" {
				// Get single resource from MinIO
				path := configPath(kind, name)
				out, err := mcExec("cat", path)
				if err != nil {
					return fmt.Errorf("%s/%s not found", kind, name)
				}
				fmt.Println(out)
			} else {
				// List all resources
				names, err := listMinIOResources(kind)
				if err != nil {
					return fmt.Errorf("failed to list %ss: %w", kind, err)
				}
				if len(names) == 0 {
					fmt.Printf("No %ss found.\n", kind)
					return nil
				}
				for _, n := range names {
					fmt.Printf("  %s/%s\n", kind, n)
				}
				fmt.Printf("Total: %d %s(s)\n", len(names), kind)
			}
			return nil
		},
	}
	return cmd
}

// --- delete ---

func deleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <resource-type> <name>",
		Short: "Delete a resource",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind := strings.TrimSuffix(args[0], "s")
			name := args[1]

			switch kind {
			case "worker", "team", "human":
			default:
				return fmt.Errorf("unknown resource type %q", args[0])
			}

			if kubeMode == "incluster" {
				// TODO: client-go delete
				fmt.Println("incluster mode: not yet implemented")
				return nil
			}

			path := configPath(kind, name)
			if _, err := mcExec("rm", path); err != nil {
				return fmt.Errorf("failed to delete %s/%s: %w", kind, name, err)
			}
			fmt.Printf("%s/%s deleted\n", kind, name)
			return nil
		},
	}
	return cmd
}

// --- YAML parsing ---

type resource struct {
	APIVersion string
	Kind       string
	Name       string
	Raw        string
}

func loadResources(files []string) ([]resource, error) {
	var resources []resource

	for _, f := range files {
		data, err := readFile(f)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", f, err)
		}

		docs := splitYAMLDocs(string(data))
		for _, doc := range docs {
			doc = strings.TrimSpace(doc)
			if doc == "" {
				continue
			}

			r := resource{Raw: doc}
			for _, line := range strings.Split(doc, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "apiVersion:") {
					r.APIVersion = strings.TrimSpace(strings.TrimPrefix(line, "apiVersion:"))
				}
				if strings.HasPrefix(line, "kind:") {
					r.Kind = strings.TrimSpace(strings.TrimPrefix(line, "kind:"))
				}
				if strings.HasPrefix(line, "name:") && r.Name == "" {
					r.Name = strings.TrimSpace(strings.TrimPrefix(line, "name:"))
				}
			}

			if r.Kind == "" || r.Name == "" {
				continue
			}
			resources = append(resources, r)
		}
	}

	return resources, nil
}

// orderForApply sorts resources: Team first, then Worker, then Human
func orderForApply(resources []resource) []resource {
	var teams, workers, humans, other []resource
	for _, r := range resources {
		switch r.Kind {
		case "Team":
			teams = append(teams, r)
		case "Worker":
			workers = append(workers, r)
		case "Human":
			humans = append(humans, r)
		default:
			other = append(other, r)
		}
	}
	result := make([]resource, 0, len(resources))
	result = append(result, teams...)
	result = append(result, workers...)
	result = append(result, humans...)
	result = append(result, other...)
	return result
}

func readFile(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

func splitYAMLDocs(content string) []string {
	var docs []string
	current := ""
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == "---" {
			if strings.TrimSpace(current) != "" {
				docs = append(docs, current)
			}
			current = ""
			continue
		}
		current += line + "\n"
	}
	if strings.TrimSpace(current) != "" {
		docs = append(docs, current)
	}
	return docs
}

func writeTempYAML(content string) (string, error) {
	f, err := os.CreateTemp("", "hiclaw-*.yaml")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	f.Close()
	return f.Name(), nil
}

// --- ZIP import ---

// applyZip converts a legacy ZIP package (manifest.json) to CRD YAML,
// uploads the ZIP to MinIO hiclaw-config/packages/, and writes the YAML
// to MinIO hiclaw-config/{kind}s/{name}.yaml.
func applyZip(zipPath string, name string, dryRun bool) error {
	// 1. Extract ZIP to temp dir
	tmpDir, err := os.MkdirTemp("", "hiclaw-zip-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	cmd := exec.Command("unzip", "-q", zipPath, "-d", tmpDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to extract ZIP: %s: %w", string(out), err)
	}

	// 2. Read manifest.json
	manifestPath := filepath.Join(tmpDir, "manifest.json")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("manifest.json not found in ZIP: %w", err)
	}

	// Simple JSON field extraction (avoid heavy dependency)
	manifestType := jsonField(string(manifestData), "type")
	if manifestType == "" {
		manifestType = "worker" // default for legacy packages
	}

	// 3. Convert manifest to CRD YAML using --name as the resource name
	var yamlContent string
	var kind string

	model := jsonField(string(manifestData), "model")
	if model == "" {
		model = "qwen3.5-plus"
	}

	switch manifestType {
	case "worker":
		kind = "worker"
		yamlContent = fmt.Sprintf(`apiVersion: hiclaw.io/v1
kind: Worker
metadata:
  name: %s
spec:
  model: %s
  package: packages/%s.zip
`, name, model, name)

	case "team":
		kind = "team"
		yamlContent = fmt.Sprintf(`apiVersion: hiclaw.io/v1
kind: Team
metadata:
  name: %s
spec:
  leader:
    name: %s-lead
    package: packages/%s.zip
  workers: []
`, name, name, name)

	default:
		return fmt.Errorf("unsupported manifest type: %s", manifestType)
	}

	if dryRun {
		fmt.Printf("Would create %s/%s from ZIP:\n", kind, name)
		fmt.Println(yamlContent)
		return nil
	}

	// 4. Upload ZIP to MinIO hiclaw-config/packages/{name}.zip
	packageDest := fmt.Sprintf("%s/hiclaw-config/packages/%s.zip", storagePrefix(), name)
	if _, err := mcExec("cp", zipPath, packageDest); err != nil {
		return fmt.Errorf("failed to upload ZIP to MinIO: %w", err)
	}
	fmt.Printf("  Package uploaded: %s\n", packageDest)

	// 5. Check if resource already exists (for create vs update message)
	yamlDest := configPath(kind, name)
	_, existErr := mcExec("stat", yamlDest)
	action := "created"
	if existErr == nil {
		action = "updated"
	}

	// 6. Write generated YAML to MinIO hiclaw-config/{kind}s/{name}.yaml
	tmpYAML, err := writeTempYAML(yamlContent)
	if err != nil {
		return fmt.Errorf("failed to write temp YAML: %w", err)
	}
	defer os.Remove(tmpYAML)

	if _, err := mcExec("cp", tmpYAML, yamlDest); err != nil {
		return fmt.Errorf("failed to upload YAML to MinIO: %w", err)
	}
	fmt.Printf("  %s/%s %s (from ZIP)\n", kind, name, action)

	return nil
}

// jsonField extracts a simple string field from JSON using jq.
// Handles both top-level and nested "worker.field" / "team.field" patterns.
func jsonField(jsonStr, field string) string {
	cmd := exec.Command("jq", "-r",
		fmt.Sprintf(`.%s // .worker.%s // .team.%s // ""`, field, field, field),
	)
	cmd.Stdin = strings.NewReader(jsonStr)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	result := strings.TrimSpace(string(out))
	if result == "null" {
		return ""
	}
	return result
}
