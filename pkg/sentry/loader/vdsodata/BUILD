load("//tools:arch.bzl", "arch_genrule")
load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

arch_genrule(
    name = "vdso_arch_linux",
    src = "//vdso",
    template = "vdso_%s.so",
    target_compatible_with = ["@platforms//os:linux"],
)

# On macOS, use pre-built VDSO binaries checked into the repo.
# vdso_arm64.so: cross-compiled via vdso/build_darwin.sh
# vdso_amd64.so: empty stub (x86_64 macOS not supported)
filegroup(
    name = "vdso_arch_macos",
    srcs = [
        "vdso_amd64.so",
        "vdso_arm64.so",
    ],
    target_compatible_with = ["@platforms//os:macos"],
)

alias(
    name = "vdso_arch",
    actual = select({
        "@platforms//os:macos": ":vdso_arch_macos",
        "//conditions:default": ":vdso_arch_linux",
    }),
)

go_library(
    name = "vdsodata",
    srcs = [
        "vdsodata.go",
        "vdsodata_amd64.go",
        "vdsodata_arm64.go",
    ],
    embedsrcs = [
        ":vdso_arch",  # keep
    ],
    marshal = False,
    stateify = False,
    visibility = ["//pkg/sentry:internal"],
)
