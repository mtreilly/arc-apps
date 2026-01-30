# arc-apps

Export installed macOS applications and Homebrew inventory.

## Features

- Export installed app bundles from /Applications
- Export user applications
- Generate Homebrew cask and formula inventories
- Output in JSON, YAML, or table format

## Installation

```bash
go install github.com/mtreilly/arc-apps@latest
```

## Usage

```bash
# Export all installed apps
arc-apps export

# Export in JSON format
arc-apps export --output json
```

## License

MIT
