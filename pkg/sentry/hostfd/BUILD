load("//tools:defs.bzl", "go_library")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

go_library(
    name = "hostfd",
    srcs = [
        "hostfd.go",
        "hostfd_darwin.go",
        "hostfd_consts.go",
        "hostfd_unsafe.go",
        "hostfd_unsafe_darwin.go",
    ],
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/safemem",
        "//pkg/sync",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
