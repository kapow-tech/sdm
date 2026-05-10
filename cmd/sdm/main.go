package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/bufbuild/protocompile"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/pluginpb"

	"github.com/kapow-tech/sdm/pkg/config"
	"github.com/kapow-tech/sdm/pkg/generator"
)

var (
	cfgFile   string
	protoFile string
	outputDir string
	version   = "dev"
)

func main() {
	cmd := newRootCmd()
	if err := cmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var rootCmd = &cobra.Command{
		Use:     "sdm",
		Short:   "SDM Tool",
		Version: getVersion(),
	}

	// sdm config
	var configCmd = &cobra.Command{
		Use:   "config",
		Short: "Generate sdm.cfg.yaml",
		RunE:  runConfig,
	}

	// sdm setup
	var setupCmd = &cobra.Command{
		Use:   "setup",
		Short: "Setup sdm tool environment",
		RunE:  runSetup,
	}

	// sdm generate
	var generateCmd = &cobra.Command{
		Use:   "generate",
		Short: "Generate SDM files from proto definitions",
		RunE:  runGenerate,
	}

	generateCmd.Flags().StringVar(&outputDir, "out", "", "Directory to output the generated files")
	generateCmd.Flags().StringVar(&protoFile, "proto", "", "Input proto file")
	generateCmd.Flags().StringVar(&cfgFile, "cfg", "sdm.cfg.yaml", "Config file sdm.cfg.yaml")

	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(setupCmd)
	rootCmd.AddCommand(generateCmd)

	return rootCmd
}

// getVersion determines the version of the tool.
// Priority:
// 1. ldflags (main.version) if set (not "dev")
// 2. debug.ReadBuildInfo().Main.Version (if known/valid)
// 3. VCS revision (pseudo-version format)
func getVersion() string {
	if version != "dev" && version != "" {
		return version
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}

	// If installed via go install (and has version tag)
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}

	// Construct pseudo-version from VCS info
	var vcsRevision string
	var vcsModified bool

	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			vcsRevision = setting.Value
		case "vcs.modified":
			vcsModified = setting.Value == "true"
		}
	}

	if vcsRevision != "" {
		// Shorten revision
		shortRev := vcsRevision
		if len(shortRev) > 12 {
			shortRev = shortRev[:12]
		}

		// Format similar to pseudo-version: v0.0.0-TIME-REV
		// Time in buildInfo is typically RFC3339. pseudo-version needs yyyymmddhhmmss.
		// However, simple RFC3339 string might be enough to distinguish or we parse it.
		// For simplicity, let's use the raw string if we can't parse easily without extra libs,
		// but Go's time package is available.

		// Let's just use the vcsTime as is or try to format it if possible.
		// But to avoid complex time parsing dependencies, we can return a formatted string.
		// v0.0.0-<rev>
		ver := fmt.Sprintf("v0.0.0-%s", shortRev)
		if vcsModified {
			ver += "-dirty"
		}
		return ver
	}

	return version
}

func runConfig(cmd *cobra.Command, args []string) error {
	content := fmt.Sprintf(`# Version of the sdm to use
sdm: "%s"

# Note: all the paths are relative to the directory containing this file

# Directory where the sdm proto files from the sdm repo are imported using buf export
# sdm-proto: "sdm/"

# List of proto files to generate
# user-protos:
#   - "invoice/invoice.proto"

# Directory where to write the generated files
# output: "gen/"

# Directory where to write the generated SQL files (defaults to output if not set)
# output-sql: "gen/sql/"
`, version)
	if err := os.WriteFile("sdm.cfg.yaml", []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write sdm.cfg.yaml: %w", err)
	}
	fmt.Println("Generated sdm.cfg.yaml")
	return nil
}

func runSetup(cmd *cobra.Command, args []string) error {
	// 1. Install required binaries
	binaries := []string{
		"google.golang.org/protobuf/cmd/protoc-gen-go@latest",
		"github.com/bufbuild/buf/cmd/buf@latest",
		"github.com/kapow-tech/sdm/cmd/protoc-gen-sdm@latest",
	}

	for _, bin := range binaries {
		parts := strings.Split(bin, "@")
		name := filepath.Base(parts[0])
		if _, err := exec.LookPath(name); err != nil {
			fmt.Printf("Installing %s...\n", name)
			installCmd := exec.Command("go", "install", bin)
			installCmd.Stdout = os.Stdout
			installCmd.Stderr = os.Stderr
			if err := installCmd.Run(); err != nil {
				return fmt.Errorf("failed to install %s: %w", name, err)
			}
		} else {
			fmt.Printf("%s is already installed.\n", name)
		}
	}

	// 2. Initialize buf module
	if _, err := os.Stat("buf.yaml"); os.IsNotExist(err) {
		fmt.Println("Initializing buf module...")
		initCmd := exec.Command("buf", "mod", "init")
		initCmd.Stdout = os.Stdout
		initCmd.Stderr = os.Stderr
		if err := initCmd.Run(); err != nil {
			return fmt.Errorf("failed to init buf: %w", err)
		}
	}

	// 3. Import sdm proto files
	cfg, err := config.LoadConfig("sdm.cfg.yaml")
	if err != nil {
		// If config missing, verify if we should just warn or fail.
		// "Dest folder is mentioned in sdm.cfg.yaml" -> implies config exists.
		return fmt.Errorf("failed to load sdm.cfg.yaml (run 'sdm config' first?): %w", err)
	}

	sdmProtoDir := cfg.SdmProto
	if sdmProtoDir == "" {
		sdmProtoDir = "sdmprotos" // default
	}

	fmt.Printf("Exporting sdm protos to %s...\n", sdmProtoDir)
	exportCmd := exec.Command("buf", "export", "https://github.com/kapow-tech/sdm.git", "--output", sdmProtoDir)
	exportCmd.Stdout = os.Stdout
	exportCmd.Stderr = os.Stderr
	if err := exportCmd.Run(); err != nil {
		return fmt.Errorf("failed to export sdm protos: %w", err)
	}

	// 4. Generate buf.work.yaml
	bufWorkContent := fmt.Sprintf(`version: v1
directories:
  - .
  - %s
`, sdmProtoDir)
	if err := os.WriteFile("buf.work.yaml", []byte(bufWorkContent), 0644); err != nil {
		return fmt.Errorf("failed to write buf.work.yaml: %w", err)
	}
	fmt.Println("Generated buf.work.yaml")

	return nil
}

func runGenerate(cmd *cobra.Command, args []string) error {
	// Parse config
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		// It has the following arguments ... --cfg, --proto and --out.
		// However, if user provides --proto and --out, they don't need config for that specific run
		// Let's treat config as optional if flags are provided".
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to load config %s: %w", cfgFile, err)
		}
		cfg = &config.Config{} // Empty config
	}

	// Determine inputs
	absCfgFile, _ := filepath.Abs(cfgFile)
	configDir := filepath.Dir(absCfgFile)
	var filesToGenerate []string

	// // Resolve Source directory
	// sourceDir := cfg.Source
	// if sourceDir != "" && !filepath.IsAbs(sourceDir) {
	// 	sourceDir = filepath.Join(configDir, sourceDir)
	// }

	if protoFile != "" {
		// We add its directory to import paths
		// and use the path for compilation. Our new resolution logic will
		// find the best relative path.
		filesToGenerate = []string{protoFile}
	} else {
		for _, p := range cfg.UserProtos {
			// 	}
			// }

			filesToGenerate = append(filesToGenerate, p)
		}
	}

	if len(filesToGenerate) == 0 {
		return fmt.Errorf("no proto files specified (use --proto or config user-protos)")
	}

	// Determine output
	out := outputDir
	if out == "" {
		out = cfg.Output
		if out != "" && !filepath.IsAbs(out) {
			out = filepath.Join(configDir, out)
		}
	}

	outSQL := cfg.OutputSQL
	if outSQL != "" && !filepath.IsAbs(outSQL) {
		outSQL = filepath.Join(configDir, outSQL)
	}
	if outSQL == "" {
		outSQL = out
	}

	sdmProtoDir := cfg.SdmProto
	if sdmProtoDir == "" {
		sdmProtoDir = "sdmprotos"
	}
	if !filepath.IsAbs(sdmProtoDir) {
		sdmProtoDir = filepath.Join(configDir, sdmProtoDir)
	}

	importPaths := []string{".", configDir, sdmProtoDir}
	// if sourceDir != "" {
	// 	// Add sourceDir to import paths, preferably early
	// 	importPaths = append([]string{sourceDir}, importPaths...)
	// }

	if protoFile != "" {
		dir := filepath.Dir(protoFile)
		if dir != "." {
			// Prepend the directory so it takes precedence
			importPaths = append([]string{dir}, importPaths...)
		}
	}

	compiler := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			ImportPaths: importPaths,
		}),
	}

	// Make filesToGenerate relative to import paths if possible
	var resolvedFilesToGenerate []string
	for _, f := range filesToGenerate {
		absF, err := filepath.Abs(f)
		if err != nil {
			resolvedFilesToGenerate = append(resolvedFilesToGenerate, f)
			continue
		}

		bestRel := f
		for _, ip := range importPaths {
			absIP, err := filepath.Abs(ip)
			if err != nil {
				continue
			}
			rel, err := filepath.Rel(absIP, absF)
			if err == nil && !strings.HasPrefix(rel, "..") {
				if resolvedFilesToGenerate == nil || len(rel) < len(bestRel) {
					bestRel = rel
				}
			}
		}
		resolvedFilesToGenerate = append(resolvedFilesToGenerate, bestRel)
	}
	filesToGenerate = resolvedFilesToGenerate

	files, err := compiler.Compile(context.Background(), filesToGenerate...)
	if err != nil {
		return fmt.Errorf("failed to compile protos: %w", err)
	}

	// ... (Rest of generation logic similar to before) ...

	req := &pluginpb.CodeGeneratorRequest{
		FileToGenerate: filesToGenerate,
		Parameter:      proto.String("paths=source_relative"),
	}

	// Collect descriptors logic
	seen := make(map[string]bool)
	var collect func(f protoreflect.FileDescriptor)
	collect = func(f protoreflect.FileDescriptor) {
		if seen[f.Path()] {
			return
		}
		seen[f.Path()] = true
		for i := 0; i < f.Imports().Len(); i++ {
			collect(f.Imports().Get(i))
		}
		req.ProtoFile = append(req.ProtoFile, protodesc.ToFileDescriptorProto(f))
	}

	for _, f := range files {
		collect(f)
	}

	opts := protogen.Options{}
	gen, err := opts.New(req)
	if err != nil {
		return fmt.Errorf("failed to create plugin: %w", err)
	}

	var firstFile *protogen.File
	for _, f := range gen.Files {
		if !f.Generate {
			continue
		}
		generator.GenerateFile(gen, f)
		if firstFile == nil {
			firstFile = f
		}
	}
	if firstFile != nil {
		generator.GenerateHelpers(gen, firstFile) // emits sdm_helpers.go once
	}

	response := gen.Response()
	if response.Error != nil {
		return fmt.Errorf("generator error: %s", response.GetError())
	}

	// Run protoc-gen-go
	if err := runProtocGenGo(req, response); err != nil {
		return fmt.Errorf("failed to run protoc-gen-go: %w", err)
	}

	for _, file := range response.File {
		name := file.GetName()
		targetDir := out
		if strings.HasSuffix(name, ".sql") {
			targetDir = outSQL
			// Flatten the path for SQL files and prepend timestamp
			name = fmt.Sprintf("%s", filepath.Base(name))
		}

		if targetDir != "" {
			name = filepath.Join(targetDir, name)
		}

		content := file.GetContent()
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(name, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to write file %s: %w", name, err)
		}
		fmt.Printf("Generated: %s\n", name)
	}

	return nil
}

func runProtocGenGo(req *pluginpb.CodeGeneratorRequest, resp *pluginpb.CodeGeneratorResponse) error {
	data, err := proto.Marshal(req)
	if err != nil {
		return err
	}

	cmd := exec.Command("protoc-gen-go")
	if _, err := exec.LookPath("protoc-gen-go"); err != nil {
		// Try to find in GOPATH/bin
		goPath := os.Getenv("GOPATH")
		if goPath == "" {
			homeDir, _ := os.UserHomeDir()
			goPath = filepath.Join(homeDir, "go")
		}
		candidate := filepath.Join(goPath, "bin", "protoc-gen-go")
		if _, err := os.Stat(candidate); err == nil {
			cmd = exec.Command(candidate)
		}
	}

	cmd.Stdin = bytes.NewReader(data)
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if os.IsNotExist(err) || err.Error() == "exec: \"protoc-gen-go\": executable file not found in $PATH" {
			return fmt.Errorf("protoc-gen-go executable not found in $PATH")
		}
		return fmt.Errorf("exec error: %v, stderr: %s", err, stderr.String())
	}

	var goResp pluginpb.CodeGeneratorResponse
	if err := proto.Unmarshal(out.Bytes(), &goResp); err != nil {
		return err
	}

	if goResp.Error != nil {
		return fmt.Errorf("protoc-gen-go error: %s", goResp.GetError())
	}

	resp.File = append(resp.File, goResp.File...)
	return nil
}
