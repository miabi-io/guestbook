package main

import (
	"context"
	"fmt"

	"github.com/jkaninda/logger"
)

// dbDisplayName maps a driver to a friendly name for the UI/seed copy.
func dbDisplayName(driver string) string {
	switch driver {
	case "sqlite", "sqlite3":
		return "SQLite"
	case "postgres":
		return "PostgreSQL"
	default:
		return driver
	}
}

func seedEntries(driver string) []struct{ Name, Message string } {
	return []struct{ Name, Message string }{
		{"Miabi", fmt.Sprintf("Welcome to your guestbook 👋 This app is running on Miabi and stores signatures in %s.", dbDisplayName(driver))},

		{"Ada", "Connected my GitHub repository, pushed code, and Miabi handled the build and deployment automatically."},
		{"Grace", "Automatic SSL certificates in a few clicks. No reverse proxy configuration required."},
		{"Linus", "Deployed a Docker image directly from a registry and it was online in under a minute."},
		{"Jude", "The built-in monitoring dashboard makes it easy to keep an eye on application health."},
		{"Dennis", "Provisioned a PostgreSQL database and attached it to my application without touching Docker commands."},
		{"Ken", "I like that every feature is available through the API as well as the web interface."},
		{"Barbara", "GitOps support means my infrastructure and applications stay reproducible."},
		{"Tim", "Multi-tenant workspaces make shared hosting on Docker feel safe and organized."},
		{"James", "Imported an existing container into Miabi and started managing it from the dashboard."},
		{"Josh", "Backups, databases, deployments, and domains all managed from a single place."},
		{"Brendan", "The built-in container registry integrates nicely with CI/CD pipelines."},
		{"Sophie", "Added a custom domain and Miabi configured routing and TLS automatically."},
		{"Alex", "Deploying from a marketplace template was much faster than setting everything up manually."},
		{"Nadia", "Workspace isolation makes it easy to host applications for multiple teams on one server."},
		{"Chris", "Connected a remote node through the agent and expanded capacity without opening inbound ports."},
		{"Anonymous", "Finally, an open-source self-hosted PaaS that feels simple without sacrificing power."},
	}
}

// Seed populates an empty wall with sample signatures.
func Seed(ctx context.Context, store *Store, driver string) error {
	return store.Exclusive(ctx, func(tx *Store) error {
		count, err := tx.Count(ctx)
		if err != nil {
			return err
		}
		if count > 0 {
			logger.Info("seed skipped, entries already present", "count", count)
			return nil
		}
		entries := seedEntries(driver)
		for _, e := range entries {
			if _, err := tx.Create(ctx, e.Name, e.Message, "seed", ""); err != nil {
				return err
			}
		}
		logger.Info("database seeded", "entries", len(entries))
		return nil
	})
}
