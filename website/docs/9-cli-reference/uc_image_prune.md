# uc image prune

Remove unused images on machines in the cluster.

## Synopsis

Remove unused images on machines in the cluster.

By default, all unused images are removed. Use --dangling-only to remove only
dangling images.

```
uc image prune [flags]
```

## Examples

```
  # Prune all unused images on all machines.
  uc image prune

  # Prune only dangling images on all machines.
  uc image prune --dangling-only

  # Prune only dangling images on specific machines without confirmation.
  uc image prune --dangling-only -m machine1,machine2 --yes
```

## Options

```
  -d, --dangling-only     Remove only dangling images.
  -h, --help              help for prune
  -m, --machine strings   Machine names or IDs to prune images on. Can be specified multiple times or as a comma-separated list.
                          If not specified, images are pruned on all machines.
  -y, --yes               Do not prompt for confirmation before pruning images.
```

## Options inherited from parent commands

```
      --connect string          Connect to a remote cluster machine without using the Uncloud configuration file. [$UNCLOUD_CONNECT]
                                Format: [ssh://]user@host[:port], ssh+go://user@host[:port], tcp://host:port, or unix:///path/to/uncloud.sock
  -c, --context string          Name of the cluster context to use (default is the current context). [$UNCLOUD_CONTEXT]
      --uncloud-config string   Path to the Uncloud configuration file. [$UNCLOUD_CONFIG] (default "~/.config/uncloud/config.yaml")
```

## See also

* [uc image](uc_image.md)	 - Manage images on machines in the cluster.

