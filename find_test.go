package main

import (
	"bytes"
	"fmt"
	"slices"
	"testing"

	"github.com/spf13/cobra"
)

// testContainers is a small catalogue, sorted as loadCatalogue sorts.
var testContainers = []container{
	{tool: "afni", release: "21.2.00", buildDate: "20210714", categories: []string{"functional imaging"}},
	{tool: "freesurfer", release: "7.4.1", buildDate: "20230101"},
	{tool: "fsl", release: "6.0.3", buildDate: "20200905", graphical: true,
		categories: []string{"image registration", "functional imaging"}},
	{tool: "fsl", release: "6.0.4", buildDate: "20210105", graphical: true},
	{tool: "fsl", release: "6.0.4_arm64", buildDate: "20260101", hidden: true},
}

func TestMatchContainers(t *testing.T) {
	tests := []struct {
		name  string
		words []string
		all   bool
		want  []string
	}{
		{name: "no words", want: []string{"afni/21.2.00", "freesurfer/7.4.1", "fsl/6.0.3", "fsl/6.0.4"}},
		{name: "one word", words: []string{"fsl"}, want: []string{"fsl/6.0.3", "fsl/6.0.4"}},
		{name: "every word must match, ignoring case", words: []string{"FSL", "6.0.4"},
			want: []string{"fsl/6.0.4"}},
		{name: "hidden with --all", words: []string{"fsl/6.0.4"}, all: true,
			want: []string{"fsl/6.0.4", "fsl/6.0.4_arm64"}},
		{name: "no match", words: []string{"spm"}},
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
	rows := testContainers[:3]
	tests := []struct {
		name     string
		terminal bool
		want     string
	}{
		{name: "tab-separated when piped",
			want: "afni/21.2.00\tfunctional imaging\t\n" +
				"freesurfer/7.4.1\t\t\n" +
				"fsl/6.0.3\timage registration, functional imaging\tyes\n"},
		{name: "aligned with a header on a terminal", terminal: true,
			want: fmt.Sprintf("%-18s%-40s%s\n", "NAME", "CATEGORIES", "GUI") +
				fmt.Sprintf("%-18s%-40s%s\n", "afni/21.2.00", "functional imaging", "") +
				fmt.Sprintf("%-18s%-40s%s\n", "freesurfer/7.4.1", "", "") +
				fmt.Sprintf("%-18s%-40s%s\n", "fsl/6.0.3", "image registration, functional imaging", "yes")},
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

	tests := []struct {
		name          string
		toComplete    string
		want          []string
		wantDirective cobra.ShellCompDirective
	}{
		{name: "every tool", want: []string{"afni/", "freesurfer/", "fsl/"}, wantDirective: toolDirective},
		{name: "tools by prefix", toComplete: "f", want: []string{"freesurfer/", "fsl/"},
			wantDirective: toolDirective},
		{name: "versions of a tool, hidden left out", toComplete: "fsl/",
			want:          []string{"fsl/6.0.3\tbuilt 20200905", "fsl/6.0.4\tbuilt 20210105"},
			wantDirective: cobra.ShellCompDirectiveNoFileComp},
		{name: "versions by prefix", toComplete: "fsl/6.0.4", want: []string{"fsl/6.0.4\tbuilt 20210105"},
			wantDirective: cobra.ShellCompDirectiveNoFileComp},
		{name: "no match", toComplete: "spm", wantDirective: toolDirective},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, directive := completeNames(testContainers, tt.toComplete)
			if !slices.Equal(got, tt.want) {
				t.Errorf("suggestions = %q, want %q", got, tt.want)
			}
			if directive != tt.wantDirective {
				t.Errorf("directive = %d, want %d", directive, tt.wantDirective)
			}
		})
	}
}
