#!/usr/bin/env python3
"""Compile factual model names from local vendor catalogs; performs no network I/O.

Download the sources listed in hostname-sources.json separately. Extract the XML
from Dell/HP CAB files with an archive tool; never execute driver packages.
"""
import argparse
import collections
import csv
import hashlib
import io
import json
from pathlib import Path
import re
import xml.etree.ElementTree as ET

REPOSITORY = Path(__file__).resolve().parents[1]
PACKAGE = REPOSITORY / "third_party/lima/pkg/guestidentity"
SOURCES = {
    "Lenovo": "https://download.lenovo.com/cdrt/td/catalogv2.xml",
    "Dell": "https://downloads.dell.com/catalog/DriverPackCatalog.cab",
    "HP": "https://hpia.hpcloud.hp.com/downloads/driverpackcatalog/HPClientDriverPackCatalog.cab",
}


def local_tag(element):
    return element.tag.rsplit("}", 1)[-1]


def read_xml(path):
    raw = path.read_bytes()
    if len(raw) > 20_000_000 or b"<!DOCTYPE" in raw.upper() or b"<!ENTITY" in raw.upper():
        raise ValueError("Unsupported XML size or declarations")
    return ET.fromstring(raw), hashlib.sha256(raw).hexdigest()


def compact(text):
    return re.sub("[^A-Z0-9]", "", text.upper())


def prefix(vendor, name, kind):
    upper = name.upper()
    if kind == "motherboard":
        core = upper.split()[0]
        suffix = next((short for word, short in [
            ("TAICHI", "TC"), ("STEEL LEGEND", "SL"), ("LIVEMIXER", "LM"),
            ("CHALLENGER", "CH"), ("LIGHTNING", "LT"), ("RIPTIDE", "RP"),
            ("NOVA", "NV"), ("ROCK", "RK"), ("PRO RS", "PRS"), ("PRO-A", "PA"),
        ] if word in upper), "")
        result = compact(core + suffix)
    elif vendor == "Lenovo":
        family, tail = upper.split(" ", 1)
        # Machine-type suffixes distinguish catalog SKUs, not the name users
        # normally give the PC. Preserve them in model metadata, not the alias.
        tail = re.split(r"\bTYPES?\b", tail, maxsplit=1)[0]
        tail = re.sub(r"(\d+)(?:ST|ND|RD|TH)\s+GEN", r"G\1", tail)
        tail = re.sub(r"\bGEN\s+(\d+)", r"G\1", tail)
        for word, short in [("CARBON", "C"), ("YOGA", "Y"), ("EXTREME", "E")]:
            tail = tail.replace(word, short)
        tail = re.sub(r"\b(INTEL|AMD|QUALCOMM|SNAPDRAGON|2-IN-1)\b", "", tail)
        # Keep model and generation together, e.g. T14G4 / X1CG12.
        result = compact(tail)
        if not result or not result[0].isalpha():
            result = {"THINKPAD": "TP", "THINKCENTRE": "TC", "THINKSTATION": "TS"}[family] + result
    elif vendor == "Dell":
        codes = re.findall(r"\b[A-Z]{2,4}\d{4,5}\b", upper)
        if codes:
            result = codes[-1]
        else:
            families = [("LATITUDE", "LAT"), ("LATITUE", "LAT"), ("OPTIPLEX", "OP"), ("PRECISION", "PRE"), ("XPS", "XPS")]
            family = next((short for word, short in families if word in upper), "DELL")
            # Latitude E5270 / Precision T7600 include a model letter before
            # the number. Do not lose it to a duplicate family-name fallback.
            numbers = re.findall(r"(?<![0-9])[0-9]{3,4}(?![0-9])", upper)
            tail = re.sub(r"^(?:DELL )?(?:LATITUDE|LATITUE|OPTIPLEX|PRECISION|XPS)\s*", "", upper)
            result = family + (numbers[-1] if numbers else compact(tail.split()[0]))
    elif vendor == "HP":
        families = [("ELITEBOOK", "EB"), ("PROBOOK", "PB"), ("ELITEDESK", "ED"), ("PRODESK", "PD"), ("DRAGONFLY", "DF"), ("ZBOOK", "ZB"), ("ELITE", "EL"), ("PRO", "PRO")]
        family = next((short for word, short in families if word in upper), "HP")
        generation = re.search(r"\bG\d+[A-Z]*\b", upper)
        model = re.search(r"\b\d{1,4}\b", upper)
        if not model:
            model = re.search(r"\b(?:Z|T)\d{1,4}\b", upper)
        core = model.group() if model else ""
        generation_text = generation.group() if generation else ""
        result = family + core + generation_text
        if len(result) > 7 and core:
            # E840G11 is clearer than cutting the model number in EB840G11.
            short = family[:1] + core + generation_text
            result = short if len(short) <= 7 else family + core
        if result in ("HP", "EB", "PB", "ED", "PD", "DF", "ZB", "EL", "PRO"):
            result += compact(upper.removeprefix("HP "))
    else:
        raise ValueError("Unknown vendor")
    result = compact(result)
    if len(result) > 7:
        # Preserve both the recognizable beginning and model/generation tail.
        result = result[:4] + result[-3:]
    if not re.fullmatch(r"[A-Z][A-Z0-9]{1,6}", result):
        raise ValueError("Invalid derived hostname prefix")
    return result


def compile_catalog(args):
    entries = set()
    sources = []
    for vendor, path in [("Lenovo", args.lenovo), ("Dell", args.dell), ("HP", args.hp)]:
        root, digest = read_xml(path)
        sources.append({"vendor": vendor, "url": SOURCES[vendor], "xml_sha256": digest})
        if vendor == "Lenovo":
            for model in root.iter("Model"):
                name = " ".join(model.attrib.get("name", "").split())
                kind = next((kind for family, kind in [("ThinkPad ", "laptop"), ("ThinkCentre ", "desktop"), ("ThinkStation ", "workstation")] if name.startswith(family)), None)
                if kind and "tablet" not in name.lower():
                    entries.add((kind, vendor, name, prefix(vendor, name, kind)))
        elif vendor == "Dell":
            kinds = {"OP": "desktop", "LAT": "laptop", "PRE": "workstation", "XPSNOTEBOOK": "laptop", "DL": "laptop", "DPMD": "desktop", "DPML": "laptop", "DPRL": "laptop", "DPD": "desktop", "DPL": "laptop", "DD": "desktop", "DEL": "laptop"}
            for brand in root.iter():
                if local_tag(brand) != "Brand" or brand.attrib.get("prefix") not in kinds:
                    continue
                kind = kinds[brand.attrib["prefix"]]
                for model in brand.iter():
                    if local_tag(model) != "Model":
                        continue
                    name = " ".join(model.attrib.get("name", "").split())
                    if name:
                        entries.add((kind, vendor, name, prefix(vendor, name, kind)))
        else:
            kinds = {"Desktops": "desktop", "Notebooks & Tablets": "laptop", "Workstations": "workstation"}
            for model in root.iter():
                if local_tag(model) != "ProductOSDriverPack":
                    continue
                fields = {local_tag(e): (e.text or "").strip() for e in model}
                name = " ".join(fields.get("SystemName", "").split())
                kind = kinds.get(fields.get("ProductType"))
                if kind and name and "tablet" not in name.lower():
                    entries.add((kind, vendor, name, prefix(vendor, name, kind)))
    raw = args.boards.read_bytes()
    boards = json.loads(raw)
    sources.append({"vendor": boards["vendor"], "url": boards["source_url"], "input_sha256": hashlib.sha256(raw).hexdigest()})
    for name in boards["models"]:
        entries.add(("motherboard", boards["vendor"], name, prefix(boards["vendor"], name, "motherboard")))
    if len(entries) < 1000:
        raise ValueError("Unexpectedly small vendor catalog")
    output = io.StringIO(newline="")
    writer = csv.writer(output, lineterminator="\n")
    writer.writerow(["kind", "vendor", "model", "prefix"])
    writer.writerows(sorted(entries))
    data = output.getvalue().encode("utf-8")
    manifest = {
        "snapshot_date": args.snapshot_date,
        "catalog_sha256": hashlib.sha256(data).hexdigest(),
        "models": len(entries),
        "unique_prefixes": len({e[3] for e in entries}),
        "by_vendor": dict(sorted(collections.Counter(e[1] for e in entries).items())),
        "by_kind": dict(sorted(collections.Counter(e[0] for e in entries).items())),
        "sources": sources,
    }
    (args.output / "hardware.csv").write_bytes(data)
    (args.output / "hostname-sources.json").write_text(json.dumps(manifest, indent=2, ensure_ascii=False) + "\n")
    # Only counts are printed; model lists remain in the reviewable output files.
    print(json.dumps({key: manifest[key] for key in ["models", "unique_prefixes", "by_vendor", "by_kind"]}))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for vendor in ["lenovo", "dell", "hp"]:
        parser.add_argument("--" + vendor, type=Path, required=True)
    parser.add_argument("--boards", type=Path, default=REPOSITORY / "scripts/data/hostname-boards.json")
    parser.add_argument("--snapshot-date", required=True)
    parser.add_argument("--output", type=Path, default=PACKAGE)
    args = parser.parse_args()
    if not re.fullmatch(r"\d{4}-\d{2}-\d{2}", args.snapshot_date):
        parser.error("snapshot-date must be YYYY-MM-DD")
    compile_catalog(args)
