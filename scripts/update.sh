#!/bin/bash

echoerr() {
    echo "$@" 1>&2
}

check_card() {
    device="${1}/device"

    vendorid=$(cat "${device}/vendor")
    if [ "$vendorid" != "0x8086" ]; then
        echoerr "Ignoring non-Intel device at $device"
        return
    fi

    mei_count=$(find "$device"/*.mei-gscfi.* 2> /dev/null | wc -l)
    if [ "$mei_count" -eq 0 ]; then
        echoerr "Ignoring device without MEI at $device"
        return
    fi

    deviceid=$(cat "${device}/device")
    if [ -n "$devid" ] && [ "$deviceid" != "$devid" ]; then
        echoerr "Ignoring device with ID $deviceid"
        return
    fi

    echoerr "Found device with ID: $deviceid"

    basename "$(readlink "$device")"
}

# detect devices that have correct vendor and device id, and have an existing mei interface.
# Output is the BDF of the device.
detect_devices() {
    local devid=$1

    echoerr "Detecting devices: ${devid}"

    [ "x" != "x${FAKE_BDFS}" ] && {
        echoerr "Using fake BDFs from environment variable: ${FAKE_BDFS}"
        for bdf in ${FAKE_BDFS}; do
            echo "$bdf"
        done

        return
    }

    # sysfs directories have specific format, so using find here is fine.
    # shellcheck disable=SC2044
    for card in $(find /sys/class/drm -maxdepth 1 -regex '.*/card[0-9]+'); do
        check_card "$card"
    done
}

update_firmware_amc() {
    local type=$1
    local filepath=$2

    echo "Calling xpu-smi: xpu-smi updatefw -y -t $type -f $filepath"

    xpu-smi updatefw -y -t "$type" -f "$filepath" || {
        echoerr "AMC firmware update failed"
        return 1
    }
}

update_firmware() {
    local deviceid=$1
    local type=$2
    local filepath=$3

    bdfIDs=$(detect_devices "$deviceid")

    if [ -z "$bdfIDs" ]; then
        echoerr "No matching devices found for device ID: $deviceid"
        return 1
    fi

    for bdf in $bdfIDs; do
        local extras=""
        if [ "$type" = "GFX" ]; then
            echoerr "Forcing GFX firmware update"
            extras="--force"
        fi

        echo "Calling xpu-smi: xpu-smi updatefw -y -d $bdf -t $type -f $filepath $extras"

        xpu-smi updatefw -y -d "$bdf" -t "$type" -f "$filepath" $extras || {
            echoerr "Firmware update failed for device $bdf"
            return 1
        }
    done
}

deviceid=$1
shift
type=$1
shift
filepath=$1
shift

if [ -z "$type" ] || [ -z "$filepath" ]; then
    echoerr "Usage: $0 <pciDeviceID> <firmwareType> <firmwareFilePath>"
    exit 1
fi

if [ "$type" != "AMC" ] && [ -z "$deviceid" ]; then
    echoerr "Usage: $0 <pciDeviceID> <firmwareType> <firmwareFilePath>"
    exit 1
fi

if [ ! -f "$filepath" ]; then
    echoerr "Firmware file $filepath does not exist"
    exit 1
fi

if [ "$type" = "AMC" ]; then
    update_firmware_amc "$type" "$filepath" || exit 1
else
    update_firmware "$deviceid" "$type" "$filepath" || exit 1
fi
