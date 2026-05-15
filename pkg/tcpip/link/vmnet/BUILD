load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "vmnet",
    srcs = ["endpoint.go"],
    stateify = False,
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/buffer",
        "//pkg/log",
        "//pkg/tcpip",
        "//pkg/tcpip/header",
        "//pkg/tcpip/stack",
    ],
)
