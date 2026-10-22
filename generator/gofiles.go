package generator

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// CopyOtherFiles copies all non-template files from "templates/dal" in templateFS into outputDir.
// A "template file" here is defined as any file with a ".tmpl" extension.
// It overwrites everything except if the file is named "serverprovider.yaml" and already exists.
func (g *Generator) CopyOtherFiles(outputDir string) error {
	// 1) Define source and destination directories
	sourceDir := "templates/dal"
	destDir := filepath.Join(outputDir) // if you want everything in outputDir/dal, do filepath.Join(outputDir, "dal")

	// 2) Walk the embedded folder "templates/dal"
	err := fs.WalkDir(templateFS, sourceDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		// Skip the root directory itself
		if path == sourceDir {
			return nil
		}

		// Compute relative path from "templates/dal"
		relPath, relErr := filepath.Rel(sourceDir, path)
		if relErr != nil {
			return relErr
		}
		outPath := filepath.Join(destDir, relPath)

		// If directory, create it
		if d.IsDir() {
			if mkErr := os.MkdirAll(outPath, 0755); mkErr != nil {
				return fmt.Errorf("failed to create subdirectory %q: %w", outPath, mkErr)
			}
			return nil
		}

		// 3) Skip files ending with ".tmpl"
		if strings.HasSuffix(d.Name(), ".tmpl") {
			return nil
		}

		// Strip ".static" suffix when copying files that shouldn't be compiled inside templates/dal
		// (e.g., schema_embed.gen.go.static -> schema_embed.gen.go)
		outPath = strings.TrimSuffix(outPath, ".static")

		if getFileNameWithoutExt(d.Name()) == "serverprovider" || getFileNameWithoutExt(d.Name()) == "db_config.example" {
			if _, statErr := os.Stat(outPath); statErr == nil {
				// The file exists, so do not overwrite
				return nil
			}
		}

		// 5) Copy the file from embedded FS to disk
		inFile, openErr := templateFS.Open(path)
		if openErr != nil {
			return fmt.Errorf("failed to open %q in templateFS: %w", path, openErr)
		}
		defer inFile.Close()

		// Make sure destination directory exists (for nested files)
		if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
			return fmt.Errorf("failed to create parent directories for %s: %w", outPath, err)
		}

		outFile, createErr := os.Create(outPath)
		if createErr != nil {
			return fmt.Errorf("failed to create file %q: %w", outPath, createErr)
		}
		defer outFile.Close()

		if _, copyErr := io.Copy(outFile, inFile); copyErr != nil {
			return fmt.Errorf("failed to copy file to %q: %w", outPath, copyErr)
		}

		// fmt.Printf("created %s\n", outPath)

		return nil
	})

	if err != nil {
		return fmt.Errorf("error walking 'templates/dal': %w", err)
	}

	return nil
}

func getFileNameWithoutExt(filePath string) string {
	filename := filepath.Base(filePath)       // Get "report.txt"
	ext := filepath.Ext(filename)             // Get ".txt"
	name := filename[:len(filename)-len(ext)] // Remove extension
	return name
}
