#!/bin/sh

# SPDX-FileCopyrightText: Copyright The Lima Authors
# SPDX-License-Identifier: Apache-2.0

# The host opens many short SSH sessions at once when an instance starts
# (port forwards, file sharing, plugins and readiness checks). With the
# default MaxStartups 10:30:100, sshd randomly drops new connections once ten
# are unauthenticated, and the host sees "exit status 255".

set -eux

[ -d /etc/ssh/sshd_config.d ] || exit 0

conf=/etc/ssh/sshd_config.d/10-max-startups.conf
want="MaxStartups 64:30:128"
if [ "$(cat "$conf" 2>/dev/null)" = "$want" ]; then
	exit 0
fi
echo "$want" >"$conf"

if [ -f /sbin/openrc-run ]; then
	if [ -f /etc/init.d/ssh ]; then
		rc-service --ifstarted ssh reload
	elif [ -f /etc/init.d/sshd ]; then
		rc-service --ifstarted sshd reload
	fi
elif command -v systemctl >/dev/null 2>&1; then
	if systemctl -q is-active ssh; then
		systemctl reload ssh
	elif systemctl -q is-active sshd; then
		systemctl reload sshd
	fi
fi
