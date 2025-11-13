Nomad Systemd Driver Plugin
==========

A production-ready [Nomad task driver plugin](https://www.nomadproject.io/docs/drivers/index.html) for managing existing systemd units on Linux systems.

This driver allows Nomad to orchestrate existing systemd services without running or supervising processes directly. It integrates with systemd via DBus and streams logs from the systemd journal.

- Website: [https://www.nomadproject.io](https://www.nomadproject.io)
- Mailing list: [Google Groups](http://groups.google.com/group/nomad-tool)

## Features

- **Lifecycle Management**: Start, stop, and monitor systemd units
- **Real-time State Tracking**: Monitor unit state changes via DBus
- **Log Streaming**: Continuous log streaming from systemd journal
- **Task Recovery**: Recover tasks after driver restart
- **Health Checking**: Automatic fingerprinting and systemd availability detection

## Requirements

- Linux system with systemd
- [Go](https://golang.org/doc/install) v1.22 or later (to compile the plugin)
- [Nomad](https://www.nomadproject.io/downloads.html) v1.10+ (to run the plugin)
- systemd development headers:
  - Debian/Ubuntu: `sudo apt-get install libsystemd-dev`
  - RHEL/CentOS/Fedora: `sudo dnf install systemd-devel`
  - Arch Linux: `sudo pacman -S systemd`

## Building the Plugin

### Using Dev Container (Recommended for macOS/Windows)

This project includes a dev container configuration for easy cross-platform development:

1. Install [Docker](https://www.docker.com/get-started) and [VS Code](https://code.visualstudio.com/)
2. Install the [Dev Containers extension](https://marketplace.visualstudio.com/items?itemName=ms-vscode-remote.remote-containers)
3. Open the project in VS Code
4. Click "Reopen in Container" when prompted (or use Command Palette: "Dev Containers: Reopen in Container")
5. Once inside the container, build the plugin:

```sh
make build
```

The dev container provides:
- Ubuntu 22.04 with systemd
- Go 1.22+
- All required systemd development libraries
- Proper Linux environment for building and testing

### Building on Linux

Clone the repository:

```sh
git clone https://github.com/kirychuk/nomad-systemd-driver-plugin.git
cd nomad-systemd-driver-plugin
```

Build the plugin:

```sh
make build
```

Or manually:

```sh
mkdir -p bin && go build -o bin/nomad-driver-systemd .
```

## Installation

1. Build the plugin (see above)
2. Copy the binary to your Nomad plugin directory:

```sh
sudo cp bin/nomad-driver-systemd /opt/nomad/plugins/
```

3. Configure Nomad to load the plugin by creating a configuration file:

```hcl
# /etc/nomad.d/systemd.hcl
plugin "systemd" {
  config {
    # Driver-level configuration (none required)
  }
}
```

4. Restart Nomad:

```sh
sudo systemctl restart nomad
```

5. Verify the driver is loaded:

```sh
nomad node status -self | grep systemd
```

## Usage

### Task Configuration

The systemd driver accepts a single required parameter:

- `unit` (string, required) - The name of the systemd unit to manage

### Example Job

```hcl
job "nginx" {
  datacenters = ["dc1"]
  type = "service"

  group "web" {
    count = 1

    task "nginx" {
      driver = "systemd"

      config {
        unit = "nginx.service"
      }

      resources {
        cpu    = 500
        memory = 256
      }
    }
  }
}
```

### Managing Custom Units

```hcl
job "myapp" {
  datacenters = ["dc1"]

  group "app" {
    task "myapp" {
      driver = "systemd"

      config {
        unit = "myapp.service"
      }
    }
  }
}
```

### Supported Unit Types

While the driver is typically used with `.service` units, it can manage any systemd unit type:

- `myapp.service` - Service units
- `mytimer.timer` - Timer units
- `mypath.path` - Path units
- `mysocket.socket` - Socket units

## How It Works

1. **StartTask**: Validates that the specified unit exists, then calls `systemctl start <unit>` via DBus
2. **StopTask**: Calls `systemctl stop <unit>` via DBus
3. **TaskStatus**: Queries the unit's `ActiveState` and `SubState` from systemd
4. **TaskWait**: Monitors unit state changes and returns when the unit becomes inactive or failed
5. **TaskLogs**: Streams logs from the systemd journal filtered by the unit name

## State Mapping

The driver maps systemd unit states to Nomad task states:

| Systemd State  | Nomad State                     |
|----------------|---------------------------------|
| `active`       | `TaskStateRunning`              |
| `activating`   | `TaskStateRunning`              |
| `inactive`     | `TaskStateExited`               |
| `failed`       | `TaskStateExited` (exit code 1) |
| `deactivating` | `TaskStateRunning`              |

## Limitations

This driver is intentionally minimal and does not implement:

- Process execution or supervision (use systemd's existing capabilities)
- Resource isolation (CPU, memory limits - configure in systemd unit files)
- Filesystem isolation or templating
- Environment variable injection (use systemd's `Environment=` directive)
- Network configuration (use systemd's network settings)
- Signal handling (signals are sent via systemd, not directly)
- Exec/command execution in running tasks

## Security Considerations

- The driver requires access to the systemd DBus socket
- On most systems, this requires running Nomad with appropriate permissions
- Ensure your systemd units are properly secured with appropriate user/group settings
- Use systemd's security features like `PrivateTmp`, `ProtectSystem`, etc.

## Troubleshooting

### Driver not detected

Check Nomad logs:
```sh
sudo journalctl -u nomad -f
```

Verify systemd is accessible:
```sh
systemctl status
```

### Unit not found

Verify the unit exists:
```sh
systemctl list-units --all | grep <unit-name>
systemctl list-unit-files | grep <unit-name>
```

### Logs not streaming

Check journal accessibility:
```sh
journalctl -u <unit-name> -f
```

## Development

### Project Structure

```
.
├── main.go              # Plugin entry point
├── driver/
│   ├── driver.go        # Main driver implementation
│   ├── config.go        # Configuration and schemas
│   └── task.go          # Task handle and log streaming
├── go.mod               # Go module definition
└── README.md            # This file
```

### Testing

To test the plugin, you'll need a Linux system with systemd:

1. Create a simple test service:

```sh
sudo tee /etc/systemd/system/test-nomad.service > /dev/null <<EOF
[Unit]
Description=Test Service for Nomad Systemd Driver

[Service]
Type=simple
ExecStart=/bin/sh -c 'while true; do echo "Hello from systemd"; sleep 5; done'
Restart=no

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
```

2. Run Nomad in dev mode:

```sh
sudo nomad agent -dev -plugin-dir=$(pwd)
```

3. Submit a test job:

```hcl
job "test" {
  type = "service"

  group "test" {
    task "test" {
      driver = "systemd"

      config {
        unit = "test-nomad.service"
      }
    }
  }
}
```
sudo v
## License

This project is licensed under the Mozilla Public License 2.0 - see the [LICENSE](LICENSE) file for details.

## Contributing

Contributions are welcome! Please feel free to submit issues or pull requests.

## Additional Resources

- [Nomad Task Drivers](https://www.nomadproject.io/docs/drivers/index.html)
- [Nomad Plugin Development](https://www.nomadproject.io/docs/internals/plugins/index.html)
- [systemd Documentation](https://www.freedesktop.org/wiki/Software/systemd/)
- [go-systemd Library](https://github.com/coreos/go-systemd)
