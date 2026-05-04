package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"text/tabwriter"

	"github.com/urfave/cli/v3"
)

func main() {
	app := &cli.Command{
		Name:  "fleet",
		Usage: "hyperfleet CLI",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "api-url", Sources: cli.EnvVars("FLEET_API_URL"), Value: "http://localhost:8080", Usage: "hyperfleet API URL"},
			&cli.StringFlag{Name: "api-key", Sources: cli.EnvVars("API_KEY", "FLEET_API_KEY"), Usage: "API key"},
			&cli.StringFlag{Name: "ssh-host", Sources: cli.EnvVars("FLEET_SSH_HOST"), Usage: "SSH host (default: api-url host)"},
			&cli.StringFlag{Name: "ssh-port", Sources: cli.EnvVars("FLEET_SSH_PORT"), Value: "2222", Usage: "SSH port"},
			&cli.StringFlag{Name: "output", Aliases: []string{"o"}, Value: "table", Usage: "output format: table | json"},
			&cli.BoolFlag{Name: "non-interactive", Sources: cli.EnvVars("FLEET_NON_INTERACTIVE"), Usage: "disable interactive prompts (errors when args missing)"},
		},
		Commands: []*cli.Command{
			{
				Name:  "machines",
				Usage: "manage machines",
				Commands: []*cli.Command{
					{
						Name:      "create",
						Usage:     "create a new machine",
						ArgsUsage: "<image>",
						Action:    createMachine,
					},
					{
						Name:   "list",
						Usage:  "list machines",
						Action: listMachines,
					},
					{
						Name:      "get",
						Usage:     "get a single machine",
						ArgsUsage: "<id>",
						Action:    getMachine,
					},
					{
						Name:      "delete",
						Aliases:   []string{"rm"},
						Usage:     "delete a machine",
						ArgsUsage: "<id>",
						Action:    deleteMachine,
					},
					{
						Name:      "ssh",
						Usage:     "ssh into a machine via the hyperfleet gateway",
						ArgsUsage: "<id>",
						Action:    sshMachine,
					},
				},
			},
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func clientFrom(cmd *cli.Command) (*apiClient, error) {
	apiURL := cmd.String("api-url")
	apiKey := cmd.String("api-key")
	if apiKey == "" {
		return nil, fmt.Errorf("--api-key required (or set $API_KEY)")
	}
	return newClient(apiURL, apiKey), nil
}

func createMachine(ctx context.Context, cmd *cli.Command) error {
	c, err := clientFrom(cmd)
	if err != nil {
		return err
	}

	image := cmd.Args().First()
	if image == "" {
		if !isInteractive(cmd) {
			return fmt.Errorf("image argument required")
		}
		image, err = selectImage()
		if err != nil {
			return err
		}
	}

	m, err := c.Create(ctx, image)
	if err != nil {
		return err
	}
	return printOne(cmd, m)
}

func listMachines(ctx context.Context, cmd *cli.Command) error {
	c, err := clientFrom(cmd)
	if err != nil {
		return err
	}
	ms, err := c.List(ctx)
	if err != nil {
		return err
	}
	return printMany(cmd, ms)
}

func getMachine(ctx context.Context, cmd *cli.Command) error {
	c, err := clientFrom(cmd)
	if err != nil {
		return err
	}
	id, err := resolveMachineID(ctx, cmd, c, "Select a machine")
	if err != nil {
		return err
	}
	m, err := c.Get(ctx, id)
	if err != nil {
		return err
	}
	return printOne(cmd, m)
}

func deleteMachine(ctx context.Context, cmd *cli.Command) error {
	c, err := clientFrom(cmd)
	if err != nil {
		return err
	}
	id, err := resolveMachineID(ctx, cmd, c, "Select a machine to delete")
	if err != nil {
		return err
	}

	if cmd.NArg() == 0 && isInteractive(cmd) {
		ok, err := confirm(fmt.Sprintf("Delete machine %s?", id))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("aborted")
			return nil
		}
	}

	if err := c.Delete(ctx, id); err != nil {
		return err
	}
	fmt.Println("deleted")
	return nil
}

func sshMachine(ctx context.Context, cmd *cli.Command) error {
	apiKey := cmd.String("api-key")
	if apiKey == "" {
		return fmt.Errorf("--api-key required")
	}

	c, err := clientFrom(cmd)
	if err != nil {
		return err
	}
	id, err := resolveMachineID(ctx, cmd, c, "SSH into which machine?")
	if err != nil {
		return err
	}

	host := cmd.String("ssh-host")
	if host == "" {
		u, err := url.Parse(cmd.String("api-url"))
		if err != nil {
			return fmt.Errorf("parse api-url: %w", err)
		}
		host = u.Hostname()
		if host == "" {
			host = "localhost"
		}
	}
	port := cmd.String("ssh-port")
	return sshAttach(host, port, id, apiKey)
}

func resolveMachineID(ctx context.Context, cmd *cli.Command, c *apiClient, title string) (string, error) {
	if id := cmd.Args().First(); id != "" {
		return id, nil
	}
	if !isInteractive(cmd) {
		return "", fmt.Errorf("id argument required")
	}
	return selectMachineID(ctx, c, title)
}

func printOne(cmd *cli.Command, m Machine) error {
	if cmd.String("output") == "json" {
		return jsonOut(m)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "ID:\t%s\n", m.ID)
	fmt.Fprintf(w, "IMAGE:\t%s\n", m.Image)
	fmt.Fprintf(w, "STATUS:\t%s\n", m.Status)
	fmt.Fprintf(w, "CREATED:\t%s\n", m.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
	if m.StartedAt != nil {
		fmt.Fprintf(w, "STARTED:\t%s\n", m.StartedAt.Format("2006-01-02T15:04:05Z07:00"))
	}
	if m.ExitedAt != nil {
		fmt.Fprintf(w, "EXITED:\t%s\n", m.ExitedAt.Format("2006-01-02T15:04:05Z07:00"))
	}
	if m.Error != "" {
		fmt.Fprintf(w, "ERROR:\t%s\n", m.Error)
	}
	return w.Flush()
}

func printMany(cmd *cli.Command, ms []Machine) error {
	if cmd.String("output") == "json" {
		return jsonOut(ms)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tIMAGE\tSTATUS\tCREATED")
	for _, m := range ms {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			m.ID, m.Image, m.Status, m.CreatedAt.Format("2006-01-02T15:04:05Z"))
	}
	return w.Flush()
}

func jsonOut(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
