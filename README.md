# Waystone Panel

**Your infrastructure. One control point.**

Waystone Panel is a free, open-source, self-hosted game-server management platform developed by **Shard Development**.

Waystone is being built to bring game-server management, infrastructure integration, automation, monitoring, and administration together into one cohesive platform.

Whether you're running a single Linux machine, managing a community's game servers, or operating multiple hosts and Proxmox clusters, Waystone aims to give you control without locking you into a particular infrastructure provider.

> **Development status:** Waystone Panel is undergoing a fresh implementation. The features described below represent the planned v1.0 scope and are not yet available as a production-ready application.

## Why Waystone?

Managing game servers often involves multiple disconnected tools for deploying workloads, accessing consoles, editing configuration files, monitoring resources, maintaining backups, and managing users.

Waystone aims to simplify that experience without sacrificing control over the underlying infrastructure.

The project is guided by four principles:

- **Your infrastructure, your choice.** Proxmox integration is optional. Standalone Linux hosts remain a first-class deployment target.
- **One cohesive experience.** Manage game servers, users, workloads, and supporting infrastructure from a unified interface.
- **Reliability by design.** Existing game workloads should continue running even if the central controller becomes temporarily unavailable.
- **Open source and self-hosted.** Keep control of your software, data, and deployment environment.

## Planned Features

### Game Server Management

- Install, start, stop, restart, and update game servers.
- Access interactive consoles and server logs.
- Manage files, configuration, and reusable installation profiles.
- Monitor server health and resource usage.
- Manage supported game-specific player and moderation features.

**Minecraft Java Edition** is the first planned supported game. The architecture is intended to support additional games and installation profiles over time.

### Infrastructure and Runtimes

- Manage game workloads across multiple Linux hosts.
- Run workloads using native systemd services and supported container runtimes.
- Integrate with multiple Proxmox VE clusters.
- Provision and manage supported Proxmox virtual machines and LXC containers.
- Discover host capabilities and enforce resource limits.

Proxmox is **not required** to use Waystone.

### Backups and Recovery

- Create manual and scheduled game-server backups.
- Restore and clone supported game-server workloads.
- Verify backup integrity using manifests and checksums.
- Configure backup destinations and retention policies.

### Users, Permissions, and Tenancy

- Organize users and game servers into tenants.
- Delegate server management through role-based permissions.
- Apply resource quotas and access restrictions.
- Maintain administrative and security audit trails.
- Support additional authentication methods, including OpenID Connect and multi-factor authentication.

### Automation and Monitoring

- Schedule maintenance and game-server tasks.
- Monitor live and historical resource metrics.
- Configure alerts and notifications.
- Integrate with supported notification destinations.
- Provide infrastructure and game-server status information.

### WaystoneOS

**WaystoneOS** is the planned Debian-based controller appliance for Waystone Panel.

The roadmap includes experimental Proxmox LXC and VM images during alpha development, progressing toward stable appliance images for v1.0.

## Architecture

Waystone is designed around a central controller and distributed agents.

| Component | Technology | Responsibility |
|---|---|---|
| Controller | Go | API, orchestration, authorization, and operations |
| Agent | Go | Secure workload execution on Linux hosts |
| Web interface | React + TypeScript | Browser-based administration and management |
| Database | PostgreSQL | Persistent application state |
| Infrastructure integration | Proxmox VE API | Optional VM, LXC, and cluster management |
| WaystoneOS | Debian-based | Dedicated controller appliance |

The architecture prioritizes secure agent communication, durable operations, explicit authority, and reconciliation after connectivity interruptions.

Existing workloads are intended to remain operational during controller outages without agents independently assuming controller authority.

## Development Roadmap

Waystone is being developed incrementally toward a complete v1.0 release.

| Release | Focus |
|---|---|
| v0.1-alpha | First playable Minecraft Java server |
| v0.2-alpha | Runtime and lifecycle expansion |
| v0.3-alpha | Files, profiles, and configuration |
| v0.4-alpha | Backup, restore, and cloning |
| v0.5-alpha | Proxmox integration and experimental WaystoneOS |
| v0.6-alpha | Identity, multi-tenancy, permissions, and quotas |
| v0.7-alpha | Automation, monitoring, and notifications |
| v0.8-alpha | Packaging and platform compatibility |
| v0.9-alpha | Complete remaining v1.0 functionality |
| v0.10-beta | Feature freeze, security, and reliability testing |
| v0.11-beta | Release readiness and disaster recovery |
| v1.0 | Stable release with the complete approved scope |

Release milestones are based on development readiness rather than fixed dates.

**Follow the public roadmap:** https://waystonepanel.com/roadmap

## Current Development

Waystone Panel is being rebuilt from the ground up with a focus on maintainable architecture, clear documentation, and deliberate development.

The first milestone is an end-to-end vertical slice capable of installing and managing a Minecraft Java server through the controller, Linux agent, and web interface.

There are currently no production-ready releases.

## Installation

Installation instructions will be published when the first usable development release becomes available.

Waystone is not yet ready for production deployment.

## Contributing

Waystone Panel is in early development.

Contributing guidelines, development setup instructions, and issue templates will be introduced as the new implementation takes shape.

Bug reports, design discussions, and constructive feedback are welcome as the project develops.

## License

Waystone Panel's original software is licensed under the **GNU Affero General Public License v3.0 only (AGPL-3.0-only)**.

Third-party components retain their respective licenses.

See the `LICENSE` file for the complete license text once it has been added to the repository.

## Project Links

- **Website:** https://waystonepanel.com
- **Roadmap:** https://waystonepanel.com/roadmap
- **GitHub mirror:** https://github.com/sharddevs/waystone-panel
- **Developer:** Shard Development

---

**Waystone Panel** — Your infrastructure. One control point.

*Built by Shard Development.*