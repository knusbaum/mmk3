# tool.mmk — pinned local CLI dependencies for mmk.
#
# Provides one type:
#
#   tool — a versioned CLI binary that needs to be on PATH before other
#     targets can run. Freshness is "does `which $target` resolve to
#     something, and what's that binary's mtime" — deliberately just
#     `which`, not a specific expected install path, so the type doesn't
#     care how the tool got onto PATH (a local GOBIN, a project-local .bin,
#     a system package manager, whatever).
#
#     There's no default build body: "install this tool" has no sensible
#     generic implementation, since the install command is inherently
#     tool-specific. defbody tool exists only to fail loudly with a hint
#     if a tool target is declared with no body of its own.
#
# Every tool is automatically registered into the `tools` group, so a
# project can depend on "every declared tool" without enumerating them:
#
#   setup : tools
#
group tools

deftype tool into tools {
    p=$(which "$target" 2>/dev/null) || exit 1
    stat -c "%Y" "$p" 2>/dev/null || stat -f "%m" "$p" 2>/dev/null
}

defbody tool {
    echo "tool '$target' has no body — add one that installs it, e.g.:" >&2
    echo "  tool $target { go install <pkg>@<version> }" >&2
    exit 1
}
