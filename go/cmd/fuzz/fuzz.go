package main

import (
	"encoding/binary"
	"fmt"
	"github.com/pkg/errors"
	uc "github.com/unicorn-engine/unicorn/bindings/go/unicorn"
	"io/ioutil"
	"os"
	"os/exec"
	"unsafe"

	"github.com/lunixbochs/usercorn/go/cmd"
	"github.com/lunixbochs/usercorn/go/models"
)

/*
#include <stdlib.h>
#include <sys/shm.h>
#include <string.h>

void *afl_setup() {
	char *id = getenv("__AFL_SHM_ID");
	if (id == NULL) {
		return NULL;
	}
	void *afl_area = shmat(atoi(id), NULL, 0);
	if (afl_area == (void *)-1) {
		return NULL;
	}
	return afl_area;
}

*/
import "C"

var MAP_SIZE uint64 = 1 << 16
var FORKSRV_FD = 198
var aflHello = []byte{1, 2, 3, 4}

func main() {
	message := []byte("In fuzz main")
	ioutil.WriteFile("/tmp/outfile", message, 0444)

	forksrvCtrl := os.NewFile(uintptr(FORKSRV_FD), "afl_ctrl")
	forksrvStatus := os.NewFile(uintptr(FORKSRV_FD+1), "afl_status")

	c := cmd.NewUsercornCmd()
	var forkAddr *uint64
	var fuzzInterp *bool

	nofork := os.Getenv("AFL_NO_FORKSRV") == "1"

	aflArea := C.afl_setup()
	if aflArea == nil {
		panic("could not set up AFL shared memory")
	}
	fuzzMap := []byte((*[1 << 30]byte)(unsafe.Pointer(aflArea))[:])

	var lastPos uint64
	blockTrace := func(_ uc.Unicorn, addr uint64, size uint32) {
		if lastPos == 0 {
			lastPos = addr >> 1
			return
		}
		loc := (addr >> 4) ^ (addr << 8)
		loc &= MAP_SIZE - 1
		fuzzMap[loc]++
		lastPos = addr >> 1
	}
	c.SetupFlags = func() error {
		forkAddr = c.Flags.Uint64("forkaddr", 0, "wait until this address to fork and begin fuzzing")
		fuzzInterp = c.Flags.Bool("fuzzinterp", false, "controls whether fuzzing is delayed until program's main entry point")
		return nil
	}
	addHook := func(u models.Usercorn) error {
		_, err := u.HookAdd(uc.HOOK_BLOCK, blockTrace, 1, 0)
		return errors.Wrap(err, "u.HookAdd() failed")
	}
	c.RunUsercorn = func(args, env []string) error {
		var err error
		u := c.Usercorn

		/*
			hh := u.SyscallHookAdd(func(_ models.Usercorn, num int, name string, args []uint64) (uint64, bool) {
				if name == "read" {
					u.Println("stopping!")
					u.Stop()
					u.Println("...")
					u.Gate().StopLock()
					u.Println("...")
					return 0, true
				}
				return 0, false
			})

			// TODO: this should really be u.Setup, u.Start???
			err = u.Run(args, env)
			if _, ok := err.(models.ExitStatus); ok {
			} else if err != nil {
				return err
			}
			u.SyscallHookDel(hh)
		*/

		if err := addHook(u); err != nil {
			return err
		}
		if nofork {
			status := 0
			err = u.Run(args, env)
			if _, ok := err.(models.ExitStatus); ok {
			} else if err != nil {
				u.Printf("Usercorn err: %s\n", err)
				status = 257
			}
			os.Exit(status)
		}

		// save cpu and memory state
		savedCtx, err := models.ContextSave(u)
		if err != nil {
			u.Println("context save failed.")
			return err
		}

		if _, err := forksrvStatus.Write(aflHello); err != nil {
			u.Println("AFL hello failed.")
			return errors.Wrap(err, "AFL hello failed.")
		}
		var aflMsg [4]byte
		// afl forkserver loop
		for {
			fmt.Println("new loop")
			lastPos = 0
			if _, err := forksrvCtrl.Read(aflMsg[:]); err != nil {
				u.Printf("Failed to receive control signal from AFL: %s\n", err)
				return errors.Wrapf(err, "Failed to receive control signal from AFL: %s", err)
			}

			// spawn a fake child so AFL has something other than us to kill
			// monitor it and if afl kills it, stop the current emulation

			// TODO: reuse it if it's still running?
			cmd := exec.Command("/bin/cat")
			procStdin, err := cmd.StdinPipe()
			if err != nil {
				u.Printf("failed to open stdin: %s\n", err)
				return errors.Wrap(err, "failed to open stdin")
			}
			if err = cmd.Start(); err != nil {
				u.Printf("failed to spawn child: %s\n", err)
				return errors.Wrap(err, "failed to spawn child")
			}
			proc := cmd.Process

			// restore cpu and memory state
			if err := models.ContextRestore(u, savedCtx); err != nil {
				u.Println("context restore failed.")
				return err
			}

			binary.LittleEndian.PutUint32(aflMsg[:], uint32(proc.Pid))
			if _, err := forksrvStatus.Write(aflMsg[:]); err != nil {
				u.Printf("Failed to send pid to AFL: %s\n", err)
				return errors.Wrap(err, "failed to send PID to AFL")
			}

			// Goroutine to stop usercorn if afl-fuzz kills our fake process
			go func() {
				proc.Wait()
				u.Stop()
			}()

			// Run() deletes all hooks, so we need to add back each time
			if err := addHook(u); err != nil {
				return err
			}

			status := 0
			err = u.Run(args, env)
			fmt.Println("run returned", err)
			if _, ok := err.(models.ExitStatus); ok {
			} else if err != nil {
				u.Printf("Usercorn err: %s\n", err)
				status = 257
			}
			binary.LittleEndian.PutUint32(aflMsg[:], uint32(status))
			if _, err := forksrvStatus.Write(aflMsg[:]); err != nil {
				u.Printf("Failed to send status to AFL: %s\n", err)
				return errors.Wrap(err, "failed to send status to AFL")
			}

			procStdin.Close()
			proc.Kill()
			proc.Wait()
		}
	}

	c.Run(os.Args, os.Environ())
}
