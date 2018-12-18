#! /usr/bin/env bash

# This script handles distro-independent Linux post-install tasks. Currently
# that means managing the /keybase redirector mountpoint. The deb-
# and rpm-specific post-install scripts call into this after doing their
# distro-specific work, which is mainly setting up package repos for updates.
# The script also attempts to restart services if the mountpoint is not being used.

set -u

rootmount="/keybase"
krbin="/usr/bin/keybase-redirector"
rootConfigFile="/etc/keybase/config.json"
disableConfigKey="disable-root-redirector"

redirector_enabled() {
  disableRedirector="false"
  if [ -r "$rootConfigFile" ] ; then
    if keybase --standalone -c "$rootConfigFile" config get -d "$disableConfigKey" &> /dev/null ; then
      disableRedirector="$(keybase --standalone -c "$rootConfigFile" config get -d "$disableConfigKey" 2> /dev/null)"
    fi
  fi
  [ "$disableRedirector" != "true" ]
}

make_mountpoint() {
  if redirector_enabled ; then
    if ! mountpoint "$rootmount" &> /dev/null; then
      mkdir -p "$rootmount"
      chown root:root "$rootmount"
      chmod 755 "$rootmount"
    fi
  fi
}

# We use machinectl here because systemctl commands do not work in either a su shell or with systemd-run --uid.
exec_as() {
    user=$1
    shift
    machinectl -q shell "$user"@ "$@"
}

# Exits with 0 iff the given user is running the service with systemd
systemd_unit_active_for() {
    user=$1
    service=$2

    # exit with 1 if command fails (for example, if not using systemd)
    active_status="$(exec_as "$user" /usr/bin/systemctl --user is-active "$service")" || return "$?"

    # exit code lost by machinectl, so grep stdout
    # use -w so "active" needle doesn't match "inactive"
    echo "$active_status" | grep -wq "active"
}

systemctl_restart_as() {
    user=$1
    service=$2
    echo "Restarting $service for $user..."
    exec_as "$user" /usr/bin/systemctl --user restart "$service"
}

systemd_safe_restart_services() {
    pids="$(pidof /usr/bin/keybase)"
    while read -r pid; do
        user="$(ps -o user= -p "$pid")"

        if systemd_unit_active_for "$user" "kbfs.service"; then
            # KBFS running, need to check mounts before proceeding

            # Don't use `mount(8)` to find kbfs mounts in case there are non-kbfs fuse systems
            mount="$(exec_as "$user" /usr/bin/keybase config get -b mountdir | tr -d '\r')"
            uses="$(exec_as "$user" /usr/bin/lsof "$mount")"
            if [ -n "$uses" ]; then
                using_programs="$(echo "$uses" | tail -n +2 | awk '{print $1}' | tr '\n' ', ')"
                echo "KBFS mount $mount for user $user currently in use by ($using_programs)."
                echo "$uses"
                echo "Aborting Keybase autorestart for $user."
                echo "Please restart manually by stopping the processes accessing KBFS and then running 'run_keybase' as $user."
                continue
            fi
        fi
        if systemd_unit_active_for "$user" "keybase.service"; then
            systemctl_restart_as "$user" "keybase.service"
        else
            # User running keybase without systemd: give instructions on restarting.
            echo "Keybase upgraded. Please restart Keybase manually by running 'run_keybase' as $user."
        fi
        if systemd_unit_active_for "$user" "keybase.gui.service"; then
            systemctl_restart_as "$user" "keybase.gui.service"
        fi
        if systemd_unit_active_for "$user" "kbfs.service"; then
            systemctl_restart_as "$user" "kbfs.service"
        fi
    done <<< "$pids"
}

if redirector_enabled ; then
  chown root:root "$krbin"
  chmod 4755 "$krbin"
else
  # Turn off suid if root has been turned off.
  chmod a-s "$krbin"
fi

currlink="$(readlink "$rootmount")"
if [ -n "$currlink" ] ; then
    # Follow the symlink one level deep if needed, to account for the
    # mount1 link.
    nextlink="$(readlink "$currlink")"
    if [ -n "$nextlink" ]; then
        currlink="$nextlink"
    fi

    # Upgrade from a rootlink-based build.
    if rm "$rootmount" &> /dev/null ; then
        echo Replacing old $rootmount symlink.
    fi
elif [ -d "$rootmount" ] ; then
    # Handle upgrading from old builds that don't have the rootlink.
    currowner="$(stat -c %U "$rootmount")"
    if [ "$currowner" != "root" ]; then
        # Remove any existing legacy mount.
        echo Unmounting $rootmount...
        if killall kbfsfuse &> /dev/null ; then
            echo Shutting down kbfsfuse...
        fi
        rmdir "$rootmount"
        echo You must run run_keybase to restore file system access.
    elif ! redirector_enabled ; then
        if killall "$(basename "$krbin")" &> /dev/null ; then
            echo "Stopping existing root redirector."
        fi
    elif killall -USR1 "$(basename "$krbin")" &> /dev/null ; then
        echo "Restarting existing root redirector."
        # If the redirector is still owned by root, that probably
        # means we're sill running an old version and it needs to be
        # manually killed and restarted.  Instead, run it as the user
        # currently running kbfsfuse.
        krName="$(basename "$krbin")"
        krUser="$(ps -o user= -C "$krName" 2> /dev/null | head -1)"
        if [ "$krUser" = "root" ]; then
            newUser="$(ps -o user= -C "kbfsfuse" 2> /dev/null | head -1)"
            killall "$krName" &> /dev/null
            if [ -n "$newUser" ] && [ "$newUser" != "root" ]; then
                # Try our best to get the user's $XDG_CACHE_HOME,
                # though depending on how it's set, it might not be
                # available to su.
                userCacheHome="$(su -c "echo -n \$XDG_CACHE_HOME" - "$newUser")"
                log="${userCacheHome:-~$newUser/.cache}/keybase/keybase.redirector.log"
                su -c "nohup \"$krbin\" \"$rootmount\" &>> $log &" "$newUser"
                echo "Root redirector now running as $newUser."
            else
                # The redirector is running as root, and either root
                # is running kbfsfuse, or no one is.  In either case,
                # just make sure it restarts (since the -USR1 restart
                # won't work for older versions).
                echo "Restarting root redirector as root."
                killall "$krName" &> /dev/null
                logdir="${XDG_CACHE_HOME:-$HOME/.cache}/keybase"
                mkdir -p "$logdir"
                log="$logdir/keybase.redirector.log"
                nohup "$krbin" "$rootmount" &>> "$log" &
            fi
        fi
        t=5
        while ! mountpoint "$rootmount" &> /dev/null; do
            sleep 1
            t=$((t-1))
            if [ $t -eq 0 ]; then
                echo "Redirector hasn't started yet."
                break
            fi
       done
    fi
fi

# Make the mountpoint if it doesn't already exist by this point.
make_mountpoint

# Update the GTK icon cache, if possible.
if command -v gtk-update-icon-cache &> /dev/null ; then
  gtk-update-icon-cache -q -t -f /usr/share/icons/hicolor
fi

systemd_safe_restart_services
