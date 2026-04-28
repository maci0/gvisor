// Copyright 2025 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package stateio

import (
	"io"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/aio"
	"gvisor.dev/gvisor/pkg/sentry/hostfd"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// FDWriter implements AsyncWriter for a host file descriptor.
type FDWriter struct {
	NoRegisterClientFD

	// fd is the file descriptor. fd is immutable.
	fd int32

	// maxWriteBytes and maxRanges are AsyncWriter parameters. Both are
	// immutable.
	maxWriteBytes uint32
	maxRanges     uint32

	q        aio.Queue
	inflight []fdWrite
	cs       []aio.Completion

	// off is the offset into fd of the next write submission.
	off int64

	// fileSize will be the file's size if inflight writes complete
	// successfully.
	fileSize int64

	// reserved is the value passed to the last call to Reserve that has not
	// yet resulted in file extension.
	reserved int64
}

type fdWrite struct {
	off   int64
	done  uint64
	total uint64
	src   LocalClientRanges
}

// NewFDWriter returns an FDWriter that writes to the given host file
// descriptor. It takes ownership of the file descriptor.
//
// On darwin, O_DIRECT and Linux AIO are not available, so writes always use
// a serial GoQueue.
//
// Preconditions:
// - fd has file offset 0.
// - maxWriteBytes > 0.
// - maxRanges > 0.
// - maxParallel > 0.
func NewFDWriter(fd int32, maxWriteBytes uint64, maxRanges, maxParallel int) *FDWriter {
	if maxWriteBytes <= 0 {
		panic("invalid maxWriteBytes")
	}
	if maxRanges <= 0 {
		panic("invalid maxRanges")
	}
	if maxParallel <= 0 {
		panic("invalid maxParallel")
	}

	// macOS does not support O_DIRECT or Linux AIO. Use serial GoQueue.
	q := aio.NewSerialGoQueue(maxParallel)

	return &FDWriter{
		fd:            fd,
		maxWriteBytes: uint32(min(maxWriteBytes, uint64(linux.MAX_RW_COUNT))),
		maxRanges:     uint32(min(maxRanges, hostfd.MaxReadWriteIov)),
		q:             q,
		inflight:      make([]fdWrite, maxParallel),
		cs:            make([]aio.Completion, 0, maxParallel),
	}
}

// Close implements AsyncWriter.Close.
func (w *FDWriter) Close() error {
	w.q.Destroy()
	return unix.Close(int(w.fd))
}

// MaxWriteBytes implements AsyncWriter.MaxWriteBytes.
func (w *FDWriter) MaxWriteBytes() uint64 {
	return uint64(w.maxWriteBytes)
}

// MaxRanges implements AsyncWriter.MaxRanges.
func (w *FDWriter) MaxRanges() int {
	return int(w.maxRanges)
}

// MaxParallel implements AsyncWriter.MaxParallel.
func (w *FDWriter) MaxParallel() int {
	return len(w.inflight)
}

// AddWrite implements AsyncWriter.AddWrite.
func (w *FDWriter) AddWrite(id int, _ SourceFile, _ memmap.FileRange, srcMap []byte) {
	aio.Write(w.q, uint64(id), w.fd, w.off, srcMap)
	w.inflight[id] = fdWrite{
		off:   w.off,
		total: uint64(len(srcMap)),
		src:   LocalClientMapping(srcMap),
	}
	w.off += int64(len(srcMap))
}

// AddWritev implements AsyncWriter.AddWritev.
func (w *FDWriter) AddWritev(id int, total uint64, _ SourceFile, _ []memmap.FileRange, srcMaps []unix.Iovec) {
	aio.Writev(w.q, uint64(id), w.fd, w.off, srcMaps)
	w.inflight[id] = fdWrite{
		off:   w.off,
		total: total,
		src:   LocalClientMappings(srcMaps),
	}
	w.off += int64(total)
}

// Wait implements AsyncWriter.Wait.
func (w *FDWriter) Wait(cs []Completion, minCompletions int) ([]Completion, error) {
	// Update w.fileSize assuming that all writes complete successfully.
	w.fileSize = max(w.fileSize, w.off)

retry:
	numCompletions := 0
	aioCS, err := w.q.Wait(w.cs, minCompletions)
	enqueuedAny := false
	for _, aioC := range aioCS {
		id := int(aioC.ID)
		inflight := &w.inflight[id]
		switch {
		case aioC.Result < 0:
			cs = append(cs, Completion{
				ID:  id,
				N:   inflight.done,
				Err: aioC.Err(),
			})
			numCompletions++
		case aioC.Result == 0:
			cs = append(cs, Completion{
				ID:  id,
				N:   inflight.done,
				Err: io.ErrShortWrite,
			})
			numCompletions++
		default:
			n := uint64(aioC.Result)
			done := inflight.done + n
			if done == inflight.total {
				cs = append(cs, Completion{
					ID: id,
					N:  done,
				})
				numCompletions++
			} else {
				// Need to continue the write to get a full write or error.
				inflight.off += int64(n)
				inflight.done = done
				inflight.src = inflight.src.DropFirst(n)
				if inflight.src.Mapping != nil {
					aio.Write(w.q, aioC.ID, w.fd, inflight.off, inflight.src.Mapping)
				} else {
					aio.Writev(w.q, aioC.ID, w.fd, inflight.off, inflight.src.Iovecs)
				}
				enqueuedAny = true
			}
		}
	}
	if enqueuedAny {
		minCompletions = max(minCompletions-numCompletions, 0)
		goto retry
	}
	return cs, err
}

// Reserve implements AsyncWriter.Reserve.
func (w *FDWriter) Reserve(n uint64) {
	w.reserved = int64(n)
}

// Finalize implements AsyncWriter.Finalize.
func (w *FDWriter) Finalize() error {
	return nil
}
