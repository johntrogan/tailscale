// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

import React from "react"
import * as Control from "src/components/control-components"
import { NodeData, NodeUpdaters } from "src/hooks/node-data"
import Card from "src/ui/card"
import Toggle from "src/ui/toggle"

export default function SSHView({
  readonly,
  node,
  nodeUpdaters,
}: {
  readonly: boolean
  node: NodeData
  nodeUpdaters: NodeUpdaters
}) {
  return (
    <>
      <h1 className="mb-1">Tailscale SSH server</h1>
      <p className="description mb-10">
        Run a Tailscale SSH server on this device and allow other devices in
        your tailnet to SSH into it.{" "}
        <a
          href="https://tailscale.com/kb/1193/tailscale-ssh/"
          className="text-blue-700"
          target="_blank"
          rel="noreferrer"
        >
          Learn more &rarr;
        </a>
      </p>
      <Card className="-mx-5 p-4">
        <label className="flex gap-3 items-center">
          <Toggle
            checked={node.RunningSSHServer}
            onChange={() =>
              nodeUpdaters.patchPrefs({
                RunSSHSet: true,
                RunSSH: !node.RunningSSHServer,
              })
            }
            disabled={readonly}
          />
          <div className="text-black text-sm font-medium leading-tight">
            Run Tailscale SSH server
          </div>
        </label>
      </Card>
      <Control.AdminContainer
        className="text-gray-500 text-sm leading-tight mt-3"
        node={node}
      >
        Remember to make sure that the{" "}
        <Control.AdminLink node={node} path="/acls">
          tailnet policy file
        </Control.AdminLink>{" "}
        allows other devices to SSH into this device.
      </Control.AdminContainer>
    </>
  )
}
