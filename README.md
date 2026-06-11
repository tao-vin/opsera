# opsera
A minimal SSH/SFTP operations bridge for AI coding agents, VPNs, bastion hosts, and internal servers.

It helps tools like Codex, Claude Code, and other local AI agents operate remote servers through a controlled local tunnel,
without requiring you to paste raw server credentials directly into the agent chat or runtime.

  Opsera is especially useful when servers are behind VPNs, bastion hosts, jump hosts, or internal networks where direct
  public SSH access is not available.

  ## Why

  Traditional SSH and file-transfer tools such as Xshell, SecureCRT, PuTTY, WinSCP, FileZilla, and FlashFXP were built
  primarily for human-driven workflows.

  They work well for manual operations, but they are not ideal when an AI coding agent needs to:

  - Run commands repeatedly
  - Upload or download files
  - Inspect command output
  - Continue a deployment workflow
  - Work through an existing VPN or bastion environment
  - Avoid direct exposure of raw credentials

  Opsera aims to provide a smaller, agent-friendly operations layer.

  ## What It Does

  Opsera provides:

  - Native SSH support
  - Native SFTP file transfer
  - A visible terminal interface
  - Manual command input for the human operator
  - Local encrypted credential storage
  - Agent-accessible tunnel workflows
  - Support for public SSH servers
  - Support for VPN, bastion, and internal network environments

  The goal is not to replace every feature of mature SSH clients.

  The goal is to provide a focused bridge between local AI agents and remote server operations.

  ## Typical Use Cases

  Opsera can be used for:

  - Provisioning new Linux servers
  - Installing JDK, databases, middleware, and backend services
  - Uploading large packages into internal networks
  - Deploying Spring Boot, Go, Node.js, Python, or other services
  - Running migration scripts
  - Configuring systemd services
  - Debugging remote applications
  - Operating machines behind VPNs or bastion hosts

  ## Example Workflow

  A typical workflow looks like this:

  1. Open your VPN or bastion connection manually.
  2. Configure the server connection in Opsera.
  3. Store credentials locally using encrypted storage.
  4. Let your AI coding agent use the Opsera tunnel.
  5. Watch the terminal output while the agent runs commands.
  6. Intervene manually when needed.

  In this setup, the AI agent does not need to know the raw SSH password or private key directly.

  ## Why Not Just Give SSH Credentials to the Agent?

  For public servers, direct SSH access is simple.

  However, giving an AI agent raw credentials has obvious risks:

  - Credentials may appear in prompts, logs, shell history, or tool traces.
  - The agent may accidentally expose sensitive connection details.
  - Internal servers may not be directly reachable from the agent environment.
  - VPN and bastion workflows are often interactive or desktop-based.

  Opsera keeps the credential setup local and gives the agent a controlled way to operate through an existing
  connection.

  ## Security Model

  Opsera is not a magic security boundary.

  It is a practical operations bridge.

  You should still define clear limits when using AI agents for infrastructure work.

  Recommended practices:

  - Use Opsera first on new or non-sensitive machines.
  - Avoid giving agents unrestricted access to production databases.
  - Prefer least-privilege SSH users.
  - Use separate credentials for agent-assisted workflows.
  - Review commands before running risky operations.
  - Keep sensitive data outside the agent-visible workflow.
  - For sensitive systems, let the agent build controlled internal tools instead of directly accessing raw data.

  ## Design Goals

  Opsera is designed to be:

  - Small
  - Fast
  - Local-first
  - Easy to inspect
  - Friendly to AI coding agents
  - Useful for real server operations
  - Free of unnecessary legacy UI features

  ## Non-Goals

  Opsera is not intended to be:

  - A full replacement for every SSH client
  - A complete enterprise bastion platform
  - A secrets manager
  - A production access-control system
  - A tool for bypassing organizational security policies

  Use it responsibly and only on infrastructure you are authorized to operate.

  ## Status

  This project is currently experimental.

  It was built to solve a real workflow problem: allowing local AI coding agents to help with server operations across
  public SSH, VPN, bastion, and internal network environments.

  APIs, configuration formats, and workflows may change.

  ## Roadmap

  Possible future improvements:

  - Better session persistence
  - More reliable reconnect behavior
  - File transfer progress UI
  - Safer command approval modes
  - Per-host permission profiles
  - Audit logs
  - Agent-specific command channels
  - Jump host configuration
  - More documentation and examples

  ## Contributing

  If you are using AI coding agents for infrastructure work, VPN environments, bastion hosts, or internal server
  deployment workflows, feedback is especially useful.

  ## Disclaimer

  Opsera can execute commands and transfer files on remote servers.

  Use it carefully.

  You are responsible for the systems you connect to, the credentials you store, and the commands you allow agents or
  users to run.
