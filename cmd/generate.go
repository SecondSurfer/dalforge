package cmd

import (
	"dalforge/generator"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// generateCmd represents the 'generate' command.
var generateCmd = &cobra.Command{
	Use:   "generate [input-directory] [output-directory]",
	Short: "Generate .gen.go, _impl.gen.go, and .sql files from YAML definitions",
	Long: `This command scans the specified input directory for YAML (.yaml) files,
then generates the corresponding contract (.gen.go), implementation (_impl.gen.go), and .sql destination file paths. 
If no input directory is provided, the current directory is used.
If no output directory is provided, it defaults to the same directory as input.`,
	Args: cobra.RangeArgs(0, 2),
	Run:  generate,
}

func generate(cmd *cobra.Command, args []string) {
	var inputDir, outputDir string

	// Determine the input directory
	if len(args) > 0 {
		inputDir = args[0]
	} else {
		// Default to current directory if not provided
		dir, err := os.Getwd()
		if err != nil {
			log.Fatalf("Failed to get current directory: %v", err)
		}
		inputDir = dir
	}

	// Determine the output directory
	if len(args) > 1 {
		outputDir = args[1]
	} else {
		// Default to input directory
		outputDir = inputDir
	}

	// Read the input directory for .yaml files
	entries, err := os.ReadDir(inputDir)
	if err != nil {
		log.Fatalf("Failed to read directory '%s': %v", inputDir, err)
	}

	// Instantiate the generator ONCE outside the loop
	gen, err := generator.NewGenerator()
	if err != nil {
		log.Fatalf("Failed creating generator %v", err)
	}

	hadDalFile := false
	for _, entry := range entries {
		// Only consider files (ignore directories)
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".yaml") {
			fileName := entry.Name()
			baseName := strings.TrimSuffix(fileName, ".yaml")

			// skip serverprovider and db_config files as those are not entity files.
			if strings.Contains(baseName, "serverprovider") || strings.Contains(baseName, "db_config") {
				continue
			}

			inputFile := filepath.Join(inputDir, entry.Name())
			contractGoPath := filepath.Join(outputDir, baseName+".gen.go")
			implGoPath := filepath.Join(outputDir, baseName+"_impl.gen.go")
			sqlPath := filepath.Join(outputDir, baseName+".sql")

			data, err := os.ReadFile(inputFile)
			if err != nil {
				log.Fatalf("Failed reading file: %s", err)
			}

			yamlContent := string(data)

			// Generate both Go outputs (Contract & Implementation)
			contractFile, implFile, err := gen.GenerateDAL(yamlContent)
			if err != nil {
				log.Fatalf("Failed GenerateDAL on file %s, %v", inputFile, err)
			}

			err = os.MkdirAll(outputDir, 0755)
			if err != nil {
				log.Fatalf("failed creating directory %s, %v", outputDir, err)
			}

			// Write Contract (.gen.go)
			err = os.WriteFile(contractGoPath, []byte(contractFile), 0644)
			if err != nil {
				log.Fatalf("failed writing DAL contract content to file %s, %v", contractGoPath, err)
			}
			fmt.Printf("Generated Contract:       %s\n", contractGoPath)

			// Write Implementation (_impl.gen.go)
			err = os.WriteFile(implGoPath, []byte(implFile), 0644)
			if err != nil {
				log.Fatalf("failed writing DAL impl content to file %s, %v", implGoPath, err)
			}
			fmt.Printf("Generated Implementation: %s\n", implGoPath)

			// Generate SQL file
			sqlFile, err := gen.GenerateSQL(yamlContent)
			if err != nil {
				log.Fatalf("failed GenerateSQL on file %s, %v", inputFile, err)
			}

			err = os.WriteFile(sqlPath, []byte(sqlFile), 0644)
			if err != nil {
				log.Fatalf("failed writing SQL content to file %s, %v", sqlPath, err)
			}
			fmt.Printf("Generated SQL:            %s\n", sqlPath)

			hadDalFile = true
		}
	}

	if hadDalFile {
		err = gen.CopyOtherFiles(outputDir)
		if err != nil {
			log.Fatalf("failed writing shared provider files to path %s, %v", outputDir, err)
		}
	}
}

func init() {
	rootCmd.AddCommand(generateCmd)
}
