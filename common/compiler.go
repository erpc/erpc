package common

import (
	"fmt"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/grafana/sobek"
)

func CompileTypeScript(entrypoint string) (string, error) {
	// Set up esbuild options
	buildOptions := api.BuildOptions{
		Bundle:      true,
		Write:       false,
		Format:      api.FormatIIFE,
		Target:      api.ES2020,
		Platform:    api.PlatformNode,
		GlobalName:  "exports",
		EntryPoints: []string{entrypoint},
		Sourcemap:   api.SourceMapInline,
	}

	buildOptions.Loader = map[string]api.Loader{
		".ts":   api.LoaderTS,
		".js":   api.LoaderJS,
		".json": api.LoaderJSON,
	}

	// Build the code
	result := api.Build(buildOptions)
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("build failed: %v", result.Errors)
	}

	// Extract the compiled code
	if len(result.OutputFiles) == 0 {
		return "", fmt.Errorf("no output generated")
	}

	return string(result.OutputFiles[0].Contents), nil
}

func CompileFunction(contents string) (sobek.Callable, error) {
	runtime, err := NewRuntime()
	if err != nil {
		return nil, err
	}
	result, err := runtime.Evaluate(contents)
	if err != nil {
		return nil, err
	}

	if obj, ok := result.(*sobek.Object); ok {
		if fn, ok := sobek.AssertFunction(obj); ok {
			return fn, nil
		}
	}
	return nil, fmt.Errorf("result is not a function")
}
