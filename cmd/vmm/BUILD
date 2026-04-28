load("@io_bazel_rules_go//go:def.bzl", "go_binary", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "vmm_lib",
    srcs = ["main.go"],
    bazel_cgo = True,
    bazel_clinkopts = ["-framework Hypervisor"],
    importpath = "gvisor.dev/gvisor/cmd/vmm",
    deps = [
        "//pkg/sentry/platform/hvf",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_binary(
    name = "vmm",
    embed = [":vmm_lib"],
    visibility = ["//:sandbox"],
)
