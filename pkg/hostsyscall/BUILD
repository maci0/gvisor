load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "hostsyscall",
    srcs = [
        "hostsyscall.go",
        "hostsyscall_darwin.go",
        "hostsyscall_linux_amd64.s",
        "hostsyscall_linux_arm64.s",
    ],
    visibility = ["//:sandbox"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)
