#!/usr/bin/env python3
"""Convert a US Army Urban Tactical Planner (UTP) KMZ into the same JSON schema
used by Base_Graphics.json / TF_WEED_COP_records.json / NTC_GCM_records.json so
the records can be bulk-loaded into Elasticsearch / Kibana and rendered by
DataViewer (davi-v2/src/index.html).

For each KML Placemark we emit one record per inner geometry (Point /
LineString / LinearRing / Polygon — MultiGeometry is unwrapped). Records carry:

    Id, SymbolType, Name, AbbreviatedName,
    LeftOrganisation, RightOrganisation, Occupant,
    SymbolCode (15-char MIL-STD-2525C SIDC), OperationalStatus,
    GeopoliticalAffiliation, Priority, LocationType,
    Latitude, Longitude (centroid), StaffComments, Comment,
    MessageDTG, SigAct, UnitPosture, UnitStrength,
    Category, FacCode, FeatureClass,
    Location: { '@xsi:type': 'Point'|'Line'|'Area',
                Points: { Point: [{Latitude, Longitude}, ...] } }

Usage:
    python3 kmz_utp_to_json.py <input.kmz> [<output.json>] [--ndjson] [--index NAME]
"""
from __future__ import annotations
import argparse
import datetime as _dt
import json
import re
import sys
import uuid
import xml.etree.ElementTree as ET
import zipfile

KML_NS = "http://www.opengis.net/kml/2.2"
NS = {"k": KML_NS}

# styleUrl (with the leading '#_') → (SymbolType label, neutral 15-char SIDC)
# UTP features are NOT formal MIL-STD-2525 tactical graphics, so we assign
# neutral generic SIDCs that DataViewer can still parse for affiliation
# (char 1 = 'N' → neutral green by default; callers can re-style via the
# Category field).
STYLE_MAP = {
    "_Bldg2KML":      ("Building",        "SNGPUUS-----***"),
    "_BTZ2KML_2":     ("BuiltUpZone",     "SNGPUUS-----***"),
    "_BTZ2KML_4":     ("BuiltUpZone",     "SNGPUUS-----***"),
    "_BTZ2KML_6":     ("BuiltUpZone",     "SNGPUUS-----***"),
    "_BTZ2KML_8":     ("BuiltUpZone",     "SNGPUUS-----***"),
    "_Bridge2KML_2":  ("Bridge",          "SNGPUSE-----***"),
    "_Roads":         ("Road",            "SNGPUST-----***"),
    "_Rail":          ("Rail",            "SNGPUST-----***"),
    "_Runways":       ("Runway",          "SNGPUSA-----***"),
    "_WaterA":        ("WaterArea",       "SNGPUUW-----***"),
    "_WaterL":        ("Waterway",        "SNGPUUW-----***"),
    "_Forest":        ("Forest",          "SNGPUUO-----***"),
    "_Ridgelines":    ("Ridgeline",       "SNGPUUO-----***"),
    "_VertL":         ("VerticalObstacle","SNGPUUO-----***"),
    "KMLStyler":      ("Annotation",      "SNGPU-------***"),
}
DEFAULT_SYMBOL = ("Feature", "SNGPU-------***")


# ──────────────────────────────────────────────────────────────────────────
# Helpers
# ──────────────────────────────────────────────────────────────────────────

def _strip_ns(tag: str) -> str:
    return tag.split("}", 1)[-1] if "}" in tag else tag


def _parse_html_attrs(html: str) -> dict[str, str]:
    """Parse the <th>Key</th><td>Value</td> table that UTP packs into each
    Placemark's <description>. Returns {Key: Value} (CDATA already decoded)."""
    if not html:
        return {}
    # Description is CDATA-escaped HTML — the angle brackets come through as
    # literal '<'/'>' once ET has decoded the CDATA, so a simple regex works.
    pairs = re.findall(
        r"<th[^>]*>\s*(.*?)\s*</th>\s*<td[^>]*>\s*(.*?)\s*</td>",
        html, flags=re.IGNORECASE | re.DOTALL,
    )
    out: dict[str, str] = {}
    for k, v in pairs:
        k = re.sub(r"<[^>]+>", "", k).strip()
        v = re.sub(r"<[^>]+>", "", v).strip()
        if k:
            out[k] = v
    return out


def _parse_coordinates(text: str) -> list[tuple[float, float]]:
    """KML <coordinates> are whitespace-separated 'lon,lat[,alt]' tuples."""
    pts: list[tuple[float, float]] = []
    if not text:
        return pts
    for token in text.split():
        parts = token.split(",")
        if len(parts) < 2:
            continue
        try:
            lon = float(parts[0]); lat = float(parts[1])
        except ValueError:
            continue
        if -180.0 <= lon <= 180.0 and -90.0 <= lat <= 90.0:
            pts.append((lat, lon))
    return pts


def _centroid(points: list[tuple[float, float]]) -> tuple[float | None, float | None]:
    if not points:
        return None, None
    return (sum(p[0] for p in points) / len(points),
            sum(p[1] for p in points) / len(points))


def _walk_geometries(geom_el, into: list):
    """Recursively flatten Point / LineString / LinearRing / Polygon out of any
    MultiGeometry. Each emitted entry is (kind, [(lat, lon), ...]) where kind
    is one of 'Point', 'Line', 'Area'."""
    tag = _strip_ns(geom_el.tag)
    if tag == "Point":
        coords = geom_el.findtext("k:coordinates", default="", namespaces=NS)
        pts = _parse_coordinates(coords)
        if pts:
            into.append(("Point", pts[:1]))
    elif tag == "LineString":
        coords = geom_el.findtext("k:coordinates", default="", namespaces=NS)
        pts = _parse_coordinates(coords)
        if len(pts) >= 2:
            into.append(("Line", pts))
    elif tag == "LinearRing":
        coords = geom_el.findtext("k:coordinates", default="", namespaces=NS)
        pts = _parse_coordinates(coords)
        if len(pts) >= 3:
            into.append(("Area", pts))
    elif tag == "Polygon":
        outer = geom_el.find("k:outerBoundaryIs/k:LinearRing/k:coordinates", NS)
        if outer is not None:
            pts = _parse_coordinates(outer.text or "")
            if len(pts) >= 3:
                into.append(("Area", pts))
    elif tag == "MultiGeometry":
        for child in geom_el:
            _walk_geometries(child, into)


# ──────────────────────────────────────────────────────────────────────────
# Placemark → record(s)
# ──────────────────────────────────────────────────────────────────────────

def placemark_to_records(pm, folder_name: str, dtg: str) -> list[dict]:
    style_url = (pm.findtext("k:styleUrl", default="", namespaces=NS) or "").strip()
    style_key = style_url.lstrip("#")
    sym_type, sym_code = STYLE_MAP.get(style_key, DEFAULT_SYMBOL)

    name = (pm.findtext("k:name", default="", namespaces=NS) or "").strip()
    attrs = _parse_html_attrs(pm.findtext("k:description", default="", namespaces=NS) or "")
    feature = attrs.get("Feature", "")
    if not name:
        name = feature or folder_name or sym_type

    geometries: list = []
    for child in pm:
        if _strip_ns(child.tag) in {"Point", "LineString", "LinearRing", "Polygon", "MultiGeometry"}:
            _walk_geometries(child, geometries)

    records: list[dict] = []
    for kind, pts in geometries:
        lat, lon = _centroid(pts)
        loc_block = {
            "@xsi:type": kind,
            "Points": {
                "Point": [{"Latitude": p[0], "Longitude": p[1]} for p in pts]
            },
        }
        comment_bits = []
        for k in ("FeatureDescription", "FAC_Code", "FeatureClass", "Existence",
                  "MaterialComposition", "Lanes", "RoadSurface", "SurfaceMaterial",
                  "Hydrology", "GapWidth", "Elevation", "RailTrackArrangement",
                  "Summary", "Source"):
            if attrs.get(k):
                comment_bits.append(f"{k}: {attrs[k]}")
        rec = {
            "Id": str(uuid.uuid4()),
            "SymbolType": sym_type,
            "Name": name,
            "AbbreviatedName": "",
            "LeftOrganisation": "",
            "RightOrganisation": "",
            "Occupant": "",
            "SymbolCode": sym_code,
            "OperationalStatus": attrs.get("Existence", ""),
            "GeopoliticalAffiliation": "",
            "Priority": "Medium",
            "LocationType": kind,
            "Latitude": lat,
            "Longitude": lon,
            "StaffComments": "",
            "Comment": " | ".join(comment_bits),
            "MessageDTG": dtg,
            "SigAct": "",
            "UnitPosture": "",
            "UnitStrength": "",
            "Category": folder_name or sym_type,
            "FacCode": attrs.get("FAC_Code", ""),
            "FeatureClass": attrs.get("FeatureClass", ""),
            "Feature": feature,
            "Location": loc_block,
        }
        records.append(rec)
    return records


# ──────────────────────────────────────────────────────────────────────────
# Walker
# ──────────────────────────────────────────────────────────────────────────

def collect_records(root: ET.Element) -> list[dict]:
    dtg = _dt.datetime.now(_dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.000Z")
    out: list[dict] = []

    def walk(el: ET.Element, folder: str):
        tag = _strip_ns(el.tag)
        if tag == "Folder":
            nm = el.findtext("k:name", default="", namespaces=NS) or folder
            folder = nm.strip()
        if tag == "Placemark":
            out.extend(placemark_to_records(el, folder, dtg))
            return  # Placemarks have no nested Placemarks worth descending into
        for child in el:
            walk(child, folder)

    walk(root, "")
    return out


# ──────────────────────────────────────────────────────────────────────────
# CLI
# ──────────────────────────────────────────────────────────────────────────

def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("input", help="Path to .kmz (or .kml) file")
    ap.add_argument("output", nargs="?", default=None,
                    help="Output path (defaults to <input>.json)")
    ap.add_argument("--ndjson", action="store_true",
                    help="Emit Elasticsearch bulk NDJSON instead of a JSON array")
    ap.add_argument("--index", default="utp_records",
                    help="Index name for --ndjson bulk action lines")
    args = ap.parse_args()

    # Read KML (from a KMZ or directly from a .kml).
    if args.input.lower().endswith(".kmz"):
        with zipfile.ZipFile(args.input) as zf:
            kml_name = next((n for n in zf.namelist() if n.lower().endswith(".kml")), None)
            if not kml_name:
                print(f"error: no .kml entry in {args.input}", file=sys.stderr)
                return 2
            with zf.open(kml_name) as fh:
                tree = ET.parse(fh)
    else:
        tree = ET.parse(args.input)

    records = collect_records(tree.getroot())
    out_path = args.output or (args.input.rsplit(".", 1)[0] + ".json")

    if args.ndjson:
        with open(out_path, "w", encoding="utf-8") as fh:
            for rec in records:
                fh.write(json.dumps({"index": {"_index": args.index, "_id": rec["Id"]}}) + "\n")
                fh.write(json.dumps(rec, ensure_ascii=False) + "\n")
    else:
        with open(out_path, "w", encoding="utf-8") as fh:
            json.dump(records, fh, ensure_ascii=False, indent=2)

    # Quick summary to stderr.
    by_kind: dict[str, int] = {}
    by_type: dict[str, int] = {}
    for r in records:
        by_kind[r["LocationType"]] = by_kind.get(r["LocationType"], 0) + 1
        by_type[r["SymbolType"]]   = by_type.get(r["SymbolType"], 0) + 1
    print(f"Wrote {len(records)} records → {out_path}", file=sys.stderr)
    print(f"  LocationType:  {by_kind}", file=sys.stderr)
    print(f"  SymbolType:    {by_type}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
