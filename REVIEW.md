# Revisión del proveedor Proxmox

Fecha: 16 de noviembre de 2026.
Base: `v4.1.0`, commit `7470b4bea946a4863d1fc5ce50f12f07e037802a`.
Rama local: `codex/review-usability-bugfixes`.

## Resultado

La revisión cubre configuración y CLI, esquema de Omni y descubrimiento, selección de nodos, asignación de VMIDs e IPs, aprovisionamiento y clonado, red, almacenamiento, HA, firewall, replicación, reconciliación, borrado, pruebas, documentación y publicación de contenedores.
Los archivos protobuf generados se han tratado como artefactos derivados; no se han editado manualmente.

Se ha inspeccionado en modo lectura el formulario real de la clase `proxmox-big` en Omni.
El formulario tenía secciones desordenadas, campos básicos vacíos y todas las opciones avanzadas expuestas a la vez.
El orden observado coincide con la serialización alfabética de mapas en `BuildDynamicSchema` y con el renderizador de formularios de Omni, que recorre las propiedades en el orden recibido.

Estado al terminar la revisión: los cambios estaban en el repositorio local, sin publicaciones ni modificaciones del entorno real.
La publicación posterior se prepara como `v4.1.1`; no implica desplegarla sobre el proveedor real.

## Errores corregidos

| Prioridad | Hallazgo | Corrección |
| --- | --- | --- |
| P1 | Los pools de IP recibían claves como `nic0:10.0.0.10-10.0.0.20/24`, que el analizador de direcciones no admite. | Se separan el rango y el propietario de cada NIC/bond. |
| P1 | Las IPs solo se recordaban en memoria y se intentaban reconstruir a partir de nombres de VMs y VMIDs inferiores, incluyendo VMs ajenas al pool. | Reservas persistidas en recursos Machine de Omni antes de arrancar la VM; recuperación de las asignaciones reales después de reiniciar. |
| P1 | Rangos solapados o usados en índices de NIC distintos podían asignar la misma IP. | Comprobación compartida de direcciones reservadas dentro del proveedor y validación de subred/familia. |
| P1 | Los reintentos de configuración de una VM clonada volvían a enviar órdenes de creación para discos ya existentes. | Reconciliación por volumen existente, ampliación solo cuando hace falta y conservación de discos adicionales del template. |
| P1 | El selector de un volumen Talos aceptaba cualquier disco suficientemente grande si no coincidía el serial. | Coincidencia exclusiva por serial y exclusión del disco de sistema. |
| P1 | Los parches de volúmenes tenían nombres compartidos entre máquinas. | Nombre con ID de solicitud; paso independiente que confirma su persistencia antes de arrancar. |
| P1 | Los errores al crear parches de volúmenes se ocultaban y el tamaño mínimo ocupaba todo el disco sin margen para metadatos. | Propagación de errores, margen de particionado y crecimiento al espacio disponible. |
| P1 | Una reserva de VMID hacía avanzar el siguiente candidato sin comprobar si ya existía otra VM en ese ID. | Comprobación de ocupación para cada candidato y eliminación del fallback que ignoraba errores de autorización/comprobación. |
| P1 | El aprovisionamiento HA interpretaba datos sin normalizar: podía enviar `state: none`/`group: none` o ignorar el perfil control-plane. | Los pasos del proveedor usan la misma configuración normalizada que la creación de la VM; afinidad `negative` válida. |
| P1 | Las VMs protegidas por el propio proveedor no se podían borrar durante el desaprovisionamiento. | Retirada de la protección y confirmación de la tarea antes de parar/borrar la VM. |
| P1 | Algunos `false` del firewall desaparecían por `omitempty`; la reconciliación eliminaba también grupos de seguridad ajenos al proveedor. | Escritura explícita de valores cero y eliminación limitada a reglas marcadas como propias. |
| P1 | Al actualizar etiquetas se perdía la marca que usa go-proxmox para limpiar la ISO de Cloud-Init. | Conservación de la marca interna al reemplazar etiquetas de usuario. |
| P2 | La actualización periódica podía reaplicar valores por defecto y retirar discos al observar por primera vez una VM existente. | Actualización desactivada por defecto; adopción inicial sin mutaciones; hash y snapshot al completar nuevas máquinas. |
| P2 | Cambios de red del invitado, firmware o extensiones podían quedar marcados como aplicados sin haberse ejecutado. | Comparación con el snapshot aplicado y rechazo explícito de cambios que requieren reprovisionar. |
| P2 | Cambiar el pool actualizaba el estado del proveedor pero no la pertenencia real de la VM. | Movimiento explícito mediante la API de pools antes de persistir el nuevo valor. |
| P2 | La actualización de un disco reemplazaba también opciones que no gestiona el proveedor. | Conservación de opciones ajenas y del identificador del volumen. |
| P2 | Cambiar el bus de arranque podía crear otro disco y retirar el anterior. | Comprobación previa del slot y del orden de arranque antes de mutar la VM. |
| P2 | La eliminación podía dar por ausente una VM migrada manualmente fuera de su nodo registrado. | Búsqueda de su ubicación real antes de finalizar el borrado. |
| P2 | Los selectores de almacenamiento podían elegir destinos deshabilitados o sin soporte para imágenes de VM. | Filtrado uniforme para selección automática, nombre y CEL. |
| P2 | Un MTU personalizado no activaba la generación de configuración de red del invitado. | Inclusión del MTU en la decisión de generar NoCloud v2. |
| P2 | Se aceptaban miembros de bond inexistentes/repetidos y configuración IP en sus interfaces esclavas. | Validación previa y configuración de capa 3 exclusivamente en el bond. |
| P2 | `rombar: false` y `replicate: false` no se transmitían como desactivaciones. | Valores opcionales con distinción entre ausencia y `false`; etiqueta de replicación corregida. |
| P2 | Los perfiles sobrescribían opciones booleanas explícitas del usuario. | Seguimiento de presencia de campos YAML, tanto agrupados como planos y por disco. |
| P2 | Fallos de firewall/replicación podían quedar ocultos mientras se registraba éxito. | Propagación de errores; no se adoptan ni eliminan trabajos de replicación ajenos. |
| P2 | Un mapeo PCI/USB incompatible con todos los nodos podía acabar eligiendo igualmente uno de ellos. | Error explícito cuando ningún candidato tiene todos los mapeos solicitados. |
| P2 | Una tarea vacía podía provocar un panic al esperar su finalización. | Error verificable en lugar de desreferenciar un puntero nulo. |

## Usabilidad y simplificación

- Formulario básico por defecto: General, CPU & Memory, Disks y Network.
- Formulario completo disponible con `--schema-mode advanced`.
- Orden del esquema original conservado también dentro de objetos y entradas de listas.
- Eliminación de numeración y etiquetas recargadas.
- Selección automática de almacenamiento por nodo, sin fijar como predeterminado el primer almacenamiento global descubierto.
- Valores por defecto de CPU/RAM documentados; el formulario ya no obliga a rellenar campos que el proveedor puede resolver.
- Validación temprana de URL, autenticación, rangos de VMID y parámetros incompatibles.
- Mensajes de lectura de configuración con la causa real y detección de claves desconocidas en el archivo de conexión.
- README renovado con nombres de campos correctos, manifiesto real de Omni, ejemplos y distinción entre versión publicada y cambios aún sin publicar.

El detalle del formulario se configura al arrancar el proveedor y es común a sus usuarios.
El esquema admite propiedades adicionales para conservar configuración avanzada y YAML heredado al alternar entre modos.
El renderizador de Omni pertenece a otro repositorio; aquí no se han añadido pestañas ni filtrado reactivo que ese renderizador no pueda ejecutar.

## Publicación y verificación

El workflow del fork ahora verifica pull requests y ejecuta pruebas con detector de carreras, análisis estático y compilación de ambas arquitecturas antes de publicar.
Las compilaciones de `main` usan `edge`; no reemplazan `latest`.
Las etiquetas de prerelease tampoco reemplazan `latest`.
El permiso de escritura de paquetes queda limitado al trabajo que publica.

Se han añadido 23 pruebas de regresión y validación, además de mantener la batería existente.
Cubren persistencia y conflictos de IPs, VMIDs, HA normalizada, borrado protegido, conservación de discos y opciones, idempotencia del clonado, firewall, reglas manuales, etiquetas internas, snapshots, perfiles, volúmenes, MTU, validación y orden del esquema.

Las pruebas, el detector de carreras, `go vet`, el lint completo (0 incidencias), ambas compilaciones Linux y `git diff --check` han finalizado correctamente.
También se ha comprobado que el YAML del workflow se puede analizar.

Comprobaciones ejecutadas sobre los cambios:

```sh
go test ./...
go test -race ./...
go vet ./...
golangci-lint run --timeout=5m
govulncheck ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/omni-infra-provider-proxmox
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./cmd/omni-infra-provider-proxmox
git diff --check
```

El escaneo de vulnerabilidades no encontró vulnerabilidades alcanzables ni en paquetes importados.
Detectó [GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932) en `golang.org/x/crypto@v0.58.0`, sobre el paquete OpenPGP obsoleto que este programa no importa ni llama.
El detalle del escaneo queda en `_out/review/vulnerability-scan.txt`.
Las dependencias del proyecto no se han actualizado como parte de estos cambios.

## Límites de la validación y preparación del despliegue

1. No se ha ejecutado el E2E con Proxmox anidado: requiere Linux, KVM, Docker privilegiado y credenciales de image factory que no están disponibles en este Mac.
2. La inspección del entorno real fue de lectura; el nuevo formulario y las operaciones de Proxmox se han comprobado mediante esquema, estado COSI y API simulada, sin crear ni borrar VMs reales.
3. El reconciliador lee MachineRequests; editar una Machine Class no garantiza que Omni modifique solicitudes existentes.
4. Las opciones de los desplegables describen el clúster completo; hay que seleccionar recursos disponibles en todos los nodos candidatos o limitar el nodo/grupo.
5. Las reservas de IP coordinan este proveedor y su estado en Omni; no sustituyen un IPAM externo ni coordinan proveedores con IDs distintos.
6. Un template debe llevar la versión de Talos, firmware y extensiones deseadas; las pruebas locales no certifican una plantilla concreta del entorno real.
7. Antes de actualizar un proveedor existente, completar o cancelar solicitudes que sigan en aprovisionamiento: el flujo incorpora pasos nuevos previos al arranque para persistir reservas y parches.
8. Cambiar el destino de un trabajo de replicación existente o ciertas pertenencias de reglas HA requiere una operación explícita de recreación; se devuelve un error en lugar de marcarlo falsamente como aplicado.

Para probar la nueva versión, conviene usar primero un proveedor con otro ID y un rango de VMIDs reservado para pruebas.
Los binarios locales de revisión se identifican como `v4.1.0-review`; no constituyen una versión publicada.
La actualización del proveedor real queda pendiente de elegir y validar ese despliegue.

Binarios de revisión compilados localmente:

| Arquitectura | Archivo | SHA-256 |
| --- | --- | --- |
| Linux amd64 | `_out/review/provider-linux-amd64` | `ed39180cf344c0443248c0d378283637cc529b918be246c69f0393db44632d0e` |
| Linux arm64 | `_out/review/provider-linux-arm64` | `8d355bd49b0cb111a87f437f85d7403f89fcb647dd43d2dbcbc3784087e1492e` |
