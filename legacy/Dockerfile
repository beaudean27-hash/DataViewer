FROM nginx:1.27-alpine

# Serve DaVi static assets
COPY davi-v2/src/ /usr/share/nginx/html/
COPY davi-v2/vendor/Cesium/ /usr/share/nginx/html/Cesium/
COPY davi-v2/vendor/mvt/ /usr/share/nginx/html/mvt/
COPY davi-v2/vendor/milsymbol/ /usr/share/nginx/html/milsymbol/

EXPOSE 80
