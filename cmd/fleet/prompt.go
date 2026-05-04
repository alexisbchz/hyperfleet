package main

import (
	"context"
	"fmt"
	"os"

	"github.com/charmbracelet/huh"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

const customImageOption = "(custom...)"

var imagePresets = []string{
	"docker.io/library/alpine:3.20",
	"docker.io/library/ubuntu:24.04",
	"docker.io/library/debian:12",
	"docker.io/library/python:3.13-slim",
	"docker.io/library/node:22-alpine",
	"docker.io/library/golang:1.26-alpine",
	customImageOption,
}

func isInteractive(cmd *cli.Command) bool {
	if cmd.Bool("non-interactive") {
		return false
	}
	return term.IsTerminal(int(os.Stdin.Fd()))
}

func selectImage() (string, error) {
	options := make([]huh.Option[string], 0, len(imagePresets))
	for _, img := range imagePresets {
		options = append(options, huh.NewOption(img, img))
	}

	var choice string
	if err := huh.NewSelect[string]().
		Title("Select an image").
		Options(options...).
		Value(&choice).
		Run(); err != nil {
		return "", err
	}

	if choice != customImageOption {
		return choice, nil
	}

	var custom string
	if err := huh.NewInput().
		Title("OCI image reference").
		Placeholder("docker.io/library/alpine:3.20").
		Value(&custom).
		Validate(func(s string) error {
			if s == "" {
				return fmt.Errorf("required")
			}
			return nil
		}).
		Run(); err != nil {
		return "", err
	}
	return custom, nil
}

func selectMachineID(ctx context.Context, c *apiClient, title string) (string, error) {
	machines, err := c.List(ctx)
	if err != nil {
		return "", err
	}
	if len(machines) == 0 {
		return "", fmt.Errorf("no machines")
	}

	options := make([]huh.Option[string], 0, len(machines))
	for _, m := range machines {
		label := fmt.Sprintf("%s  %-8s  %s", m.ID, m.Status, m.Image)
		options = append(options, huh.NewOption(label, m.ID))
	}

	var id string
	if err := huh.NewSelect[string]().
		Title(title).
		Options(options...).
		Value(&id).
		Run(); err != nil {
		return "", err
	}
	return id, nil
}

func confirm(title string) (bool, error) {
	var yes bool
	err := huh.NewConfirm().
		Title(title).
		Affirmative("Yes").
		Negative("No").
		Value(&yes).
		Run()
	return yes, err
}
