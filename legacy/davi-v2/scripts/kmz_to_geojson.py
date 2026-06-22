#!/usr/bin/env python3
"""Convert a KMZ file to GeoJSON."""

import zipfile
import json
import xml.etree.ElementTree as ET
import sys
import os

KML_NS = "http://www.opengis.net/kml/2.2"
GX_NS  = "http://www.google.com/kml/ext/2.2"
NS = {"kml": KML_NS, "gx": GX_NS}


def parse_coords(text):
    """Parse a KML coordinates string into a list of [lon, lat, alt?] arrays."""
    coords = []
    for token in text.strip().split():
        parts = token.split(",")
        if len(parts) >= 2:
            lon, lat = float(parts[0]), float(parts[1])
            if len(parts) >= 3 and parts[2]:
                coords.append([lon, lat, float(parts[2])])
            else:
                coords.append([lon, lat])
    return coords


def kml_geom_to_geojson(el):
    """Recursively convert a KML geometry element to a GeoJSON geometry dict."""
    tag = el.tag.split("}")[-1]

    if tag == "Point":
        coords_el = el.find("kml:coordinates", NS)
        if coords_el is None:
            return None
        pts = parse_coords(coords_el.text)
        return {"type": "Point", "coordinates": pts[0] if pts else []}

    elif tag == "LineString":
        coords_el = el.find("kml:coordinates", NS)
        if coords_el is None:
            return None
        return {"type": "LineString", "coordinates": parse_coords(coords_el.text)}

    elif tag == "LinearRing":
        coords_el = el.find("kml:coordinates", NS)
        if coords_el is None:
            return None
        return {"type": "LineString", "coordinates": parse_coords(coords_el.text)}

    elif tag == "Polygon":
        outer = el.find("kml:outerBoundaryIs/kml:LinearRing/kml:coordinates", NS)
        rings = []
        if outer is not None:
            rings.append(parse_coords(outer.text))
        for inner in el.findall("kml:innerBoundaryIs/kml:LinearRing/kml:coordinates", NS):
            rings.append(parse_coords(inner.text))
        return {"type": "Polygon", "coordinates": rings}

    elif tag == "MultiGeometry":
        geoms = []
        for child in el:
            g = kml_geom_to_geojson(child)
            if g:
                geoms.append(g)
        if not geoms:
            return None
        return {"type": "GeometryCollection", "geometries": geoms}

    return None


def placemark_to_feature(pm, folder_path):
    """Convert a KML Placemark element to a GeoJSON Feature."""
    name = pm.findtext("kml:name", "", NS)
    desc = pm.findtext("kml:description", "", NS)
    style_url = pm.findtext("kml:styleUrl", "", NS)
    visibility = pm.findtext("kml:visibility", "1", NS)

    # Extended data
    extended = {}
    ed = pm.find("kml:ExtendedData", NS)
    if ed is not None:
        for data in ed.findall("kml:Data", NS):
            k = data.get("name", "")
            v = data.findtext("kml:value", "", NS)
            if k:
                extended[k] = v
        for sdata in ed.findall("kml:SchemaData/kml:SimpleData", NS):
            k = sdata.get("name", "")
            if k and sdata.text:
                extended[k] = sdata.text

    # Find geometry
    geometry = None
    GEOM_TAGS = {"Point", "LineString", "Polygon", "MultiGeometry", "LinearRing"}
    for child in pm:
        tag = child.tag.split("}")[-1]
        if tag in GEOM_TAGS:
            geometry = kml_geom_to_geojson(child)
            break

    if geometry is None:
        return None

    properties = {
        "name": name,
        "description": desc,
        "styleUrl": style_url,
        "visibility": visibility,
        "folder": folder_path,
    }
    properties.update(extended)

    return {
        "type": "Feature",
        "geometry": geometry,
        "properties": properties,
    }


def walk_element(el, folder_path, features):
    """Walk KML Document/Folder tree and collect features."""
    tag = el.tag.split("}")[-1]

    if tag in ("kml", "Folder", "Document"):
        name = el.findtext("kml:name", "", NS)
        child_path = (folder_path + " / " + name).strip(" /") if name else folder_path
        for child in el:
            walk_element(child, child_path, features)

    elif tag == "Placemark":
        feat = placemark_to_feature(el, folder_path)
        if feat:
            features.append(feat)


def kmz_to_geojson(kmz_path, output_path):
    print(f"Reading {kmz_path} ...")
    with zipfile.ZipFile(kmz_path) as z:
        # Prefer doc.kml, fall back to first .kml entry
        kml_name = next(
            (n for n in z.namelist() if n.lower() == "doc.kml"),
            next((n for n in z.namelist() if n.lower().endswith(".kml")), None),
        )
        if not kml_name:
            print("ERROR: No KML file found inside KMZ.", file=sys.stderr)
            sys.exit(1)
        print(f"Parsing {kml_name} ...")
        with z.open(kml_name) as f:
            data = f.read()

    root = ET.fromstring(data)

    features = []
    walk_element(root, "", features)

    geojson = {
        "type": "FeatureCollection",
        "features": features,
    }

    print(f"Writing {len(features)} features to {output_path} ...")
    with open(output_path, "w", encoding="utf-8") as f:
        json.dump(geojson, f, ensure_ascii=False, indent=2)
    print("Done.")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        base = os.path.splitext(os.path.basename("/workspaces/DataViewer/UTP_Koksan.kmz"))[0]
        kmz  = "/workspaces/DataViewer/UTP_Koksan.kmz"
        out  = f"/workspaces/DataViewer/{base}.json"
    else:
        kmz = sys.argv[1]
        out = sys.argv[2] if len(sys.argv) > 2 else os.path.splitext(kmz)[0] + ".json"

    kmz_to_geojson(kmz, out)
