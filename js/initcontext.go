/*
 *
 * k6 - a next-generation load testing tool
 * Copyright (C) 2016 Load Impact
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package js

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dop251/goja"
	"github.com/loadimpact/k6/js/common"
	"github.com/loadimpact/k6/js/compiler"
	"github.com/loadimpact/k6/js/modules"
	"github.com/loadimpact/k6/loader"
	"github.com/spf13/afero"
)

type programWithSource struct {
	pgm *goja.Program
	src string
}

// Provides APIs for use in the init context.
type InitContext struct {
	// Bound runtime; used to instantiate objects.
	runtime *goja.Runtime

	// Pointer to a context that bridged modules are invoked with.
	ctxPtr *context.Context

	// Filesystem to load files and scripts from.
	fs  afero.Fs
	pwd string

	// Cache of loaded programs and files.
	programs map[string]programWithSource
	files    map[string][]byte

	// Console object.
	Console *Console
}

func NewInitContext(rt *goja.Runtime, ctxPtr *context.Context, fs afero.Fs, pwd string) *InitContext {
	return &InitContext{
		runtime: rt,
		ctxPtr:  ctxPtr,
		fs:      fs,
		pwd:     pwd,

		programs: make(map[string]programWithSource),
		files:    make(map[string][]byte),

		Console: NewConsole(),
	}
}

func newBoundInitContext(base *InitContext, ctxPtr *context.Context, rt *goja.Runtime) *InitContext {
	return &InitContext{
		runtime: rt,
		ctxPtr:  ctxPtr,

		fs:  nil,
		pwd: base.pwd,

		programs: base.programs,
		files:    base.files,

		Console: base.Console,
	}
}

func (i *InitContext) Require(arg string) goja.Value {
	switch {
	case arg == "k6", strings.HasPrefix(arg, "k6/"):
		// Builtin modules ("k6" or "k6/...") are handled specially, as they don't exist on the
		// filesystem. This intentionally shadows attempts to name your own modules this.
		v, err := i.requireModule(arg)
		if err != nil {
			common.Throw(i.runtime, err)
		}
		return v
	default:
		// Fall back to loading from the filesystem.
		v, err := i.requireFile(arg)
		if err != nil {
			common.Throw(i.runtime, err)
		}
		return v
	}
}

func (i *InitContext) requireModule(name string) (goja.Value, error) {
	mod, ok := modules.Index[name]
	if !ok {
		return nil, errors.New(fmt.Sprintf("unknown builtin module: %s", name))
	}
	return i.runtime.ToValue(common.Bind(i.runtime, mod, i.ctxPtr)), nil
}

func (i *InitContext) requireFile(name string) (goja.Value, error) {
	// Resolve the file path, push the target directory as pwd to make relative imports work.
	pwd := i.pwd
	filename := loader.Resolve(pwd, name)
	i.pwd = loader.Dir(filename)
	defer func() { i.pwd = pwd }()

	// Swap the importing scope's imports out, then put it back again.
	oldExports := i.runtime.Get("exports")
	defer i.runtime.Set("exports", oldExports)
	oldModule := i.runtime.Get("module")
	defer i.runtime.Set("module", oldModule)

	exports := i.runtime.NewObject()
	i.runtime.Set("exports", exports)
	module := i.runtime.NewObject()
	_ = module.Set("exports", exports)
	i.runtime.Set("module", module)

	// Read sources, transform into ES6 and cache the compiled program.
	pgm, ok := i.programs[filename]
	if !ok {
		data, err := loader.Load(i.fs, pwd, name)
		if err != nil {
			return goja.Undefined(), err
		}
		src, _, err := compiler.Transform(string(data.Data), data.Filename)
		if err != nil {
			return goja.Undefined(), err
		}
		pgm_, err := goja.Compile(data.Filename, src, true)
		if err != nil {
			return goja.Undefined(), err
		}
		pgm = programWithSource{pgm_, src}
		i.programs[filename] = pgm
	}

	// Execute the program to populate exports. You may notice that this theoretically allows an
	// imported file to access or overwrite globals defined outside of it. Please don't do anything
	// stupid with this, consider *any* use of it undefined behavior >_>;;
	if _, err := i.runtime.RunProgram(pgm.pgm); err != nil {
		return goja.Undefined(), err
	}

	return module.Get("exports"), nil
}

func (i *InitContext) Open(name string) (string, error) {
	filename := loader.Resolve(i.pwd, name)
	data, ok := i.files[filename]
	if !ok {
		data_, err := loader.Load(i.fs, i.pwd, name)
		if err != nil {
			return "", err
		}
		i.files[filename] = data_.Data
		data = data_.Data
	}
	return string(data), nil
}
