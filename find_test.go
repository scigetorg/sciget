package main

import (
	"bytes"
	"fmt"
	"slices"
	"testing"

	"github.com/spf13/cobra"
)

// testContainers is a small catalog, sorted as readContainers sorts.
var testContainers = []container{
	{tool: "afni", version: "21.2.00", buildDate: "20210714", categories: []string{"functional imaging"},
		catalog: "neuro"},
	{tool: "freesurfer", version: "7.4.1", buildDate: "20230101", catalog: "neuro"},
	{tool: "fsl", version: "6.0.3", buildDate: "20200905", graphical: true,
		categories: []string{"image registration", "functional imaging"}, catalog: "neuro"},
	{tool: "fsl", version: "6.0.3", buildDate: "20240101", catalog: "mylab", shadowed: true},
	{tool: "fsl", version: "6.0.4", buildDate: "20210105", graphical: true},
	{tool: "fsl", version: "6.0.4_arm64", buildDate: "20260101", hidden: true},
}

func TestMatchContainers(t *testing.T) {
	tests := []struct {
		name  string
		words []string
		all   bool
		want  []string
	}{
		{name: "no words", want: []string{"afni/21.2.00", "freesurfer/7.4.1", "fsl/6.0.3", "fsl/6.0.3", "fsl/6.0.4"}},
		{name: "one word", words: []string{"fsl"}, want: []string{"fsl/6.0.3", "fsl/6.0.3", "fsl/6.0.4"}},
		{name: "every word must match, ignoring case", words: []string{"FSL", "6.0.4"},
			want: []string{"fsl/6.0.4"}},
		{name: "hidden with --all", words: []string{"fsl/6.0.4"}, all: true,
			want: []string{"fsl/6.0.4", "fsl/6.0.4_arm64"}},
		{name: "no match", words: []string{"spm"}},
		{name: "a category matches", words: []string{"Functional"},
			want: []string{"afni/21.2.00", "fsl/6.0.3"}},
		{name: "name and category together", words: []string{"fsl", "registration"},
			want: []string{"fsl/6.0.3"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, c := range matchContainers(testContainers, tt.words, tt.all) {
				got = append(got, c.name())
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("names = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWriteContainers(t *testing.T) {
	rows := testContainers[:4]
	tests := []struct {
		name     string
		terminal bool
		want     string
	}{
		{name: "tab-separated when piped",
			want: "afni/21.2.00\tneuro\tfunctional imaging\t\t\n" +
				"freesurfer/7.4.1\tneuro\t\t\t\n" +
				"fsl/6.0.3\tneuro\timage registration, functional imaging\tyes\t\n" +
				"fsl/6.0.3\tmylab\t\t\t(shadowed)\n"},
		{name: "aligned with a header on a terminal", terminal: true,
			want: fmt.Sprintf("%-18s%-9s%-40s%-5s%s\n", "NAME", "CATALOG", "CATEGORIES", "GUI", "NOTE") +
				fmt.Sprintf("%-18s%-9s%-40s%-5s%s\n", "afni/21.2.00", "neuro", "functional imaging", "", "") +
				fmt.Sprintf("%-18s%-9s%-40s%-5s%s\n", "freesurfer/7.4.1", "neuro", "", "", "") +
				fmt.Sprintf("%-18s%-9s%-40s%-5s%s\n", "fsl/6.0.3", "neuro", "image registration, functional imaging", "yes", "") +
				fmt.Sprintf("%-18s%-9s%-40s%-5s%s\n", "fsl/6.0.3", "mylab", "", "", "(shadowed)")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeContainers(&out, rows, tt.terminal); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != tt.want {
				t.Errorf("output =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestCompleteNames(t *testing.T) {
	const toolDirective = cobra.ShellCompDirectiveNoSpace | cobra.ShellCompDirectiveNoFileComp

	// A hidden container in the first catalog shadows a visible copy.
	hiddenWinner := []container{
		{tool: "x", version: "1", buildDate: "1", catalog: "a", hidden: true},
		{tool: "x", version: "1", buildDate: "2", catalog: "b", shadowed: true},
	}
	tests := []struct {
		name          string
		containers    []container // testContainers when nil
		toComplete    string
		all           bool
		want          []string
		wantDirective cobra.ShellCompDirective
	}{
		{name: "every tool", want: []string{"afni/", "freesurfer/", "fsl/"}, wantDirective: toolDirective},
		{name: "tools by prefix", toComplete: "f", want: []string{"freesurfer/", "fsl/"},
			wantDirective: toolDirective},
		{name: "versions of a tool, hidden left out", toComplete: "fsl/",
			want:          []string{"fsl/6.0.3\tbuilt 20200905", "fsl/6.0.4\tbuilt 20210105"},
			wantDirective: cobra.ShellCompDirectiveNoFileComp},
		{name: "hidden with --all", toComplete: "fsl/", all: true,
			want: []string{"fsl/6.0.3\tbuilt 20200905", "fsl/6.0.4\tbuilt 20210105",
				"fsl/6.0.4_arm64\tbuilt 20260101"},
			wantDirective: cobra.ShellCompDirectiveNoFileComp},
		{name: "versions by prefix", toComplete: "fsl/6.0.4", want: []string{"fsl/6.0.4\tbuilt 20210105"},
			wantDirective: cobra.ShellCompDirectiveNoFileComp},
		{name: "no match", toComplete: "spm", wantDirective: toolDirective},
		{name: "hidden winner hides its shadowed copy", containers: hiddenWinner, toComplete: "x/",
			wantDirective: cobra.ShellCompDirectiveNoFileComp},
		{name: "hidden winner with --all", containers: hiddenWinner, toComplete: "x/", all: true,
			want: []string{"x/1\tbuilt 1"}, wantDirective: cobra.ShellCompDirectiveNoFileComp},
		{name: "hidden winner's tool", containers: hiddenWinner, toComplete: "x", all: true,
			want: []string{"x/"}, wantDirective: toolDirective},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			containers := tt.containers
			if containers == nil {
				containers = testContainers
			}
			got, directive := completeNames(containers, tt.toComplete, tt.all)
			if !slices.Equal(got, tt.want) {
				t.Errorf("suggestions = %q, want %q", got, tt.want)
			}
			if directive != tt.wantDirective {
				t.Errorf("directive = %d, want %d", directive, tt.wantDirective)
			}
		})
	}
}
