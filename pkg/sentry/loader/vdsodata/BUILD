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

# On macOS, create empty VDSO stubs since we can't cross-compile
# the Linux VDSO shared library.
genrule(
    name = "vdso_arch_macos",
    outs = [
        "vdso_amd64.so",
        "vdso_arm64.so",
    ],
    cmd = "touch $(location vdso_amd64.so) $(location vdso_arm64.so)",
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
