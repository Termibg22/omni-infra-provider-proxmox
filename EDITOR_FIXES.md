# Correcciones del editor e inventario: 4.1.2

La solución modifica exclusivamente el proveedor. No incluye parches, extensiones de navegador ni cambios al Omni alojado.

## Problemas y alcance real

| Problema observado | Cambio en 4.1.2 |
| --- | --- |
| Perfiles sin explicación | Ayuda detallada en el tooltip del perfil y tabla en README: cambios exactos, extensiones, requisitos y precedencia. Se conservan valores simples de enum porque los títulos distintos al valor rompen la selección guardada en Omni 1.13.3. |
| Nodo obsoleto `pve2` | `/nodes` es la fuente de pertenencia. Referencias en HA, configuración, almacenamiento o mapeos no crean nodos. Los grupos se filtran contra ese inventario. |
| `local-lvm` ofrecido para discos sin admitirlos | Solo se ofrecen almacenamientos activos, habilitados y con contenido `images` en un nodo actual. `rootdir` es para contenedores y no es suficiente. La misma comprobación se aplica al aprovisionar por nombre, CEL o selección automática. No se bloquea por nombre: un `local-lvm` válido sí puede alojar discos. |
| CPU/RAM antiguos vacíos o `got null, want integer` | Esquema plano de nuevo, compatible con `cores` y `memory` guardados. Los límites numéricos se validan dentro de subesquemas para impedir que el renderizador haga `Math.max(undefined, minimum)` y escriba NaN. Se admite `null`/0 como omisión en campos numéricos opcionales. |
| Disco/NIC antiguos aparentemente perdidos | Regresan los controles de disco de arranque y NIC principal, enlazados a los campos originales. Las listas completas se explican como reemplazos opcionales; vacías no eliminan los dispositivos originales. No se migran ni reescriben clases automáticamente. |
| Opciones avanzadas ocultas | Todas se exponen en ambos modos: firmware, CPU, discos/QoS, IPs, bonds, DNS, HA, replicación, firewall, PCI/USB y plantillas. Se recuperan también campos ausentes de bonds, NICs adicionales y log del firewall. |
| Grupo/nodo/pool inicialmente vacío | El proveedor interpreta vacío como `auto` y lo indica en la etiqueta. Omni ignora `default` en estos desplegables; el proveedor no puede forzar una selección visual por usuario. |
| Recarga pierde trabajo sin guardar | Sigue siendo una limitación del Omni alojado. El proveedor no recibe el borrador del navegador y no puede persistirlo. Una actualización de esquema sin recargar mantiene una edición en las pruebas de integración. |

Los nuevos formularios pueden mostrar 0 para números no introducidos: las etiquetas explican que usa los valores automáticos (2 cores, 4096 MiB, 20 GiB). Los valores guardados, por ejemplo 2/4096/20, se muestran directamente.
No se anuncian pestañas: el Omni alojado genera el diseño y no acepta un UI schema enviado por el proveedor.
El YAML agrupado de v4 sigue siendo válido en el backend, pero sus valores solo agrupados no aparecen en los controles planos; para esas clases debe editarse el manifiesto con `omnictl`.

## Verificación

- Pruebas Go de inventario con un nodo eliminado aún presente en recursos agregados, almacenamiento, grupos y mapeos.
- API simulada con un destino válido, uno `rootdir`, uno deshabilitado y otro inactivo.
- Detector de carreras, `go vet`, lint completo y compilación Linux amd64/arm64.
- Tres pruebas del renderizador original de Omni v1.13.3, commit `1834114db70c2bbfff71cd4ba8a1882a01504193`, sin modificar sus componentes: valores antiguos visibles e intactos, formulario nuevo sin NaN/null ni dispositivos reemplazados y edición conservada durante actualización del esquema.
- Prueba visual local usando ese editor original y datos sintéticos; no se modificó ninguna clase ni VM real.

Para repetir las pruebas de interoperabilidad, en un checkout de Omni de esa revisión con sus dependencias de frontend instaladas, copiar `hack/test/omni-ui/provider-schema.spec.ts` a `frontend/src/components/Form/ProviderSchema.spec.ts` y ejecutar desde `frontend`:

```sh
PROXMOX_PROVIDER_SCHEMA=/ruta/absoluta/al/provider/cmd/omni-infra-provider-proxmox/data/schema.json \
  npm test -- --run src/components/Form/ProviderSchema.spec.ts
```

Ese archivo es solo una prueba: no es un parche del producto ni debe incluirse en un build de Omni.
Las pruebas reales de Proxmox siguen limitadas a lectura y simulación de API; no se ha hecho E2E con creación de VMs en el clúster del usuario.
