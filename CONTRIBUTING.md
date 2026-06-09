# Contributing to homelab

Thank you for your interest in contributing! This project is in active development, and contributions are welcome.

## Ways to contribute

1. **Test services**: Deploy services from the catalog and report issues or improvements
2. **Add new services**: Follow the [service addition guide](docs/adding-a-service.md)
3. **Report bugs**: Open issues with detailed reproduction steps and logs
4. **Improve documentation**: Fix typos, clarify instructions, add examples
5. **Submit PRs**: Bug fixes, new features, and improvements are welcome

## Development setup

```bash
# Clone the repository
git clone https://github.com/you/homelab
cd homelab

# Build and install locally
make install

# Run tests
make test

# Lint and run tests with race detector
make ci

# Export catalog for local browsing
make catalog
```

## Code style

- Follow existing patterns in the codebase
- Run `go vet` and `golint` before committing
- Add tests for new functionality
- Keep PRs focused and well-described

## Adding a new service

See [docs/adding-a-service.md](docs/adding-a-service.md) for step-by-step instructions.

### Service requirements

1. **Use official images**: Prefer official Docker images from the upstream project
2. **Network isolation**: Main container on `home-services`, databases/workers on `internal: true` network
3. **Configuration schema**: Use `config.yaml` with `vars` (non-secrets) and `secrets` (keyring-stored)
4. **Caddy configs**: Provide both `caddy.conf` (private) and `caddy.cf.conf` (public)
5. **Test end-to-end**: Verify the service works from the catalog before submitting

### Service template

```
assets/services/<name>/
├── docker-compose.yml    # Service definition
├── caddy.conf            # Private reverse proxy (tailnet)
├── caddy.cf.conf        # Public reverse proxy (Cloudflare Tunnel)
└── config.yaml           # Configuration schema
```

## Testing

### Manual testing checklist

- [ ] Service installs from catalog: `homelab service add <name>`
- [ ] Configuration works: `homelab service setup <name>`
- [ ] Containers start: `homelab service up <name>`
- [ ] Private access works: `homelab service enable <name> --private`
- [ ] Public access works (if applicable): `homelab service enable <name> --public`
- [ ] Service is accessible via browser
- [ ] Logs show no errors: `homelab service logs <name>`

### Automated tests

```bash
# Run all tests
make test

# Run specific package tests
go test ./internal/service/...
```

## Reporting bugs

When reporting bugs, please include:

1. **Steps to reproduce**: Detailed steps to reproduce the issue
2. **Expected behavior**: What you expected to happen
3. **Actual behavior**: What actually happened
4. **Logs**: Relevant logs from `homelab service logs <name>` or `homelab core logs`
5. **Environment**: OS, Docker version, Go version
6. **Configuration**: Sanitized config (remove secrets)

## Pull request process

1. **Fork the repository** and create a feature branch
2. **Make your changes** following the code style guidelines
3. **Add tests** for new functionality
4. **Update documentation** if needed
5. **Test thoroughly** both manually and with automated tests
6. **Submit a PR** with a clear description of changes

### PR description template

```markdown
## Description
Brief description of what this PR does and why.

## Changes
- Change 1
- Change 2
- Change 3

## Testing
- [ ] Manual testing performed
- [ ] Automated tests added/pass
- [ ] Documentation updated

## Related issues
Closes #123
```

## Documentation

Documentation is maintained in the `docs/` directory:

- `docs/architecture.md` - Design decisions and network flow
- `docs/tailscale-setup.md` - Tailscale configuration
- `docs/cloudflare-setup.md` - Cloudflare DNS and Tunnel setup
- `docs/adding-a-service.md` - Service contribution guide

When updating documentation:
- Keep it clear and concise
- Include examples where helpful
- Update both the README and relevant docs files
- Test instructions by following them yourself

## Getting help

- **Questions**: Open a GitHub discussion
- **Bugs**: Open a GitHub issue with the template
- **Features**: Open a GitHub issue or discussion to discuss first

## License

By contributing, you agree that your contributions will be licensed under the same license as the project.
