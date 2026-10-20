/*
 * JuiceFS, Copyright 2025 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/urfave/cli/v2"
)

func cmdMkdir() *cli.Command {
	return &cli.Command{
		Name:      "mkdir",
		Action:    mkdir,
		Usage:     "create a directory in a volume",
		ArgsUsage: "META-URL PATH",
		Category:  "ADMIN",
		Description: `
Create a directory in a volume without mounting it.

The new directory inherits the object storage sub-cluster of its parent by
default; files created under it are routed to that cluster.

Examples:
# Create a directory inheriting the parent's object cluster
$ juicefs mkdir redis://localhost /dir1

# Create a directory whose files are stored in object cluster 2
$ juicefs mkdir redis://localhost /dir1 --object-id 2`,
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:  "object-id",
				Usage: "object storage sub-cluster id (0-255) for files under this directory; only supported by the TiKV engine",
				Action: func(ctx *cli.Context, v int) error {
					if v < 0 || v > int(meta.MaxObjectID) {
						return fmt.Errorf("object-id should be between 0 and %d", meta.MaxObjectID)
					}
					return nil
				},
			},
		},
	}
}

func mkdir(ctx *cli.Context) error {
	setup(ctx, 2)
	removePassword(ctx.Args().Get(0))
	m := meta.NewClient(ctx.Args().Get(0), nil)
	if _, err := m.Load(true); err != nil {
		return err
	}
	p := ctx.Args().Get(1)
	var parentIno meta.Ino
	var attr meta.Attr
	eno := m.Resolve(meta.Background(), meta.RootInode, filepath.Dir(p), &parentIno, &attr, true)
	if eno != 0 {
		return eno
	}
	c := meta.Background()
	if ctx.IsSet("object-id") {
		c = meta.WithObjectID(c, uint8(ctx.Int("object-id")))
	}
	var ino meta.Ino
	eno = m.Mkdir(c, parentIno, filepath.Base(p), 0755, 0, 1, &ino, &meta.Attr{})
	if eno != 0 {
		return eno
	}
	fmt.Printf("Created directory %s (inode %d, object cluster %d)\n", p, ino, ino.ObjectID())
	return nil
}
