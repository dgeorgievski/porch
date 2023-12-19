// Copyright 2023 The kpt and Nephio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/nephio-project/porch/cmd/porchctl/run"
	"github.com/nephio-project/porch/internal/kpt/errors"
	"github.com/nephio-project/porch/internal/kpt/errors/resolver"
	"github.com/nephio-project/porch/internal/kpt/util/cmdutil"
	"github.com/spf13/cobra"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/component-base/cli"
	"k8s.io/klog/v2"
	k8scmdutil "k8s.io/kubectl/pkg/cmd/util"
)

func main() {
	// Handle all setup in the runMain function so os.Exit doesn't interfere
	// with defer.
	os.Exit(runMain())
}

// runMain does the initial setup in order to run kpt. The return value from
// this function will be the exit code when kpt terminates.
func runMain() int {
	var err error

	ctx := context.Background()

	// Enable commandline flags for klog.
	// logging will help in collecting debugging information from users
	klog.InitFlags(nil)

	cmd := run.GetMain(ctx)

	err = cli.RunNoErrOutput(cmd)
	if err != nil {
		return handleErr(cmd, err)
	}
	return 0
}

// handleErr takes care of printing an error message for a given error.
func handleErr(cmd *cobra.Command, err error) int {
	// First attempt to see if we can resolve the error into a specific
	// error message.
	if re, resolved := resolver.ResolveError(err); resolved {
		if re.Message != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "%s \n", re.Message)
		}
		return re.ExitCode
	}

	// Then try to see if it is of type *errors.Error
	var kptErr *errors.Error
	if errors.As(err, &kptErr) {
		unwrapped, ok := errors.UnwrapErrors(kptErr)
		if ok && !cmdutil.PrintErrorStacktrace() {
			fmt.Fprintf(cmd.ErrOrStderr(), "Error: %s \n", unwrapped.Error())
			return 1
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "%s \n", kptErr.Error())
		return 1
	}

	// Finally just let the error handler for kubectl handle it. This handles
	// printing of several error types used in kubectl
	// TODO: See if we can handle this in kpt and get a uniform experience
	// across all of kpt.
	k8scmdutil.CheckErr(err)
	return 1
}
