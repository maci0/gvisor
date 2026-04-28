load("@io_bazel_rules_go//go:def.bzl", "go_binary", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "hvftest_lib",
    srcs = ["main.go"],
    cgo = True,
    clinkopts = ["-framework Hypervisor"],
    importpath = "gvisor.dev/gvisor/cmd/hvftest",
)

go_binary(
    name = "hvftest",
    embed = [":hvftest_lib"],
    visibility = ["//:sandbox"],
)
