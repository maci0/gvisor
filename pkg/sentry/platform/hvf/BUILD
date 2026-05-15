load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "hvf",
    srcs = [
        "address_space.go",
        "context.go",
        "hvf.go",
        "ipa_allocator.go",
        "kernel_pagetable.go",
        "machine.go",
        "memory_el1.go",
        "pagetable.go",
        "vcpu_arm64.go",
    ],
    bazel_cgo = True,
    bazel_clinkopts = ["-framework Hypervisor -framework CoreFoundation"],
    stateify = False,
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/context",
        "//pkg/fd",
        "//pkg/hostarch",
        "//pkg/hosttid",
        "//pkg/log",
        "//pkg/sentry/arch",
        "//pkg/sentry/memmap",
        "//pkg/sentry/platform",
        "//pkg/sentry/platform/interrupt",
        "//pkg/sync",
    ],
)

go_test(
    name = "hvf_test",
    srcs = [
        "hvf_test.go",
    ],
    library = ":hvf",
)
