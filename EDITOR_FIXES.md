# Correcciones del provider: 4.1.9

La solución modifica exclusivamente el proveedor. No incluye parches, extensiones de navegador ni cambios al Omni alojado.

## Cambios de 4.1.9

- Se reproduce el bloqueo de Destroy: la librería intenta borrar primero `user-data-<vmid>.iso`, pero el token no tiene `Datastore.Allocate` para borrar contenido del almacenamiento. El error genérico ocultaba esa operación e impedía llegar al borrado de la VM, aunque el token sí tuviera `VM.Allocate` y `VM.PowerMgmt`.
- La eliminación de la VM usa directamente la API de Proxmox con `purge=1` y `destroy-unreferenced-disks=1`. Se eliminan los discos usados/propios y los discos sin referencia con el mismo VMID en almacenamientos habilitados. Proxmox limpia sus referencias en trabajos de backup, replicación y HA.
- La limpieza de la ISO privada tiene un límite de 30 segundos y un fallo no bloquea el borrado principal. El aviso identifica el volumen, la operación y el permiso necesario. Se conservan la ISO compartida de Talos y las imágenes de otras VMs.
- Se espera el resultado de la tarea de borrado. Las pruebas con el controlador del SDK comprueban que un rechazo de permisos, una tarea fallida o la ausencia de tarea conservan la solicitud y el estado para reintentar.

Al actualizar, las solicitudes en destrucción vuelven a intentarse. Con los permisos mostrados por el usuario, puede quedar la ISO privada: eliminarla requiere `Datastore.Allocate` en su almacenamiento, tanto para el usuario como para el token si usa separación de privilegios. El provider no modifica permisos automáticamente. El cambio se verifica con una API simulada; el borrado real queda pendiente del despliegue.

## Cambios de 4.1.8

- Se reproduce el error de los logs en `recordProviderData`: la VM ya está arrancada, pero guardar el YAML aplicado como anotación falla por sus saltos de línea. La prueba utiliza la validación real de metadatos del SDK de Omni.
- La configuración aplicada pasa a `MachineSpec.applied_provider_data`, en el recurso propio del provider. Solo su hash queda en una anotación. Se corrigen las tres escrituras: aprovisionamiento, adopción de máquinas antiguas y actualización de VMs existentes.
- Se conservan las lecturas de anotaciones antiguas y las comprobaciones que rechazan cambios incompatibles en VMs existentes. Las escrituras de actualización respetan el propietario del recurso, evitando conflictos con el controlador de aprovisionamiento.
- Se prueba el controlador del SDK desde el estado `FAILED` y el checkpoint `syncHARules`: llega a `PROVISIONED`, borra el error y conserva VMID, nodo y tarea de arranque sin acceder a Proxmox. También se comprueban YAML largo, valores false explícitos, configuraciones agrupadas y persistencia protobuf.

Para recuperar solicitudes detenidas por este error, actualizar y reiniciar el provider conservando las solicitudes y las VMs existentes. La recuperación se ha verificado en pruebas; no se ha desplegado esta versión en el clúster real durante la corrección.

## Cambios de 4.1.7

- Se reprodujo el fallo en el Omni real con provider 4.1.6: Cores cambió de 8 sin guardar a 2 tras `ERR_CONNECTION_CLOSED`. La clase seguía en la misma versión guardada, y el estado del provider conservaba su versión y exactamente el mismo esquema.
- El cierre afectó simultáneamente a los streams de clases, provider, métricas y notificaciones. Se observaron cierres alrededor de 60 segundos sin actividad. No se identifica qué componente de red aplica ese límite.
- Una prueba de lectura abrió el stream existente de salud del provider, que emite eventos cada 30 segundos. El borrador permaneció intacto durante 216 segundos; al cerrar ese stream volvió a perderse. No se guardó ninguna clase ni se modificó ninguna VM.
- Se añade un latido cada 30 segundos en la anotación `proxmox.omni.siderolabs.io/editor-heartbeat` de `InfraProviderStatus`, el recurso que el editor ya observa. No cambia el esquema, no vuelve a descubrir inventario y no escribe MachineClasses. El latido de salud existente usa otro recurso que el editor no observa.
- `--editor-keepalive-interval` permite ajustar el intervalo o desactivarlo con `0`. Cada intento tiene un timeout de 10 segundos; un error se registra y no detiene el aprovisionamiento ni los intentos siguientes. No se crea un estado vacío durante el arranque ni se resucita uno en destrucción.

Es una mitigación del cierre por inactividad observado. Un corte real de red, una recarga manual o la caducidad de sesión siguen pudiendo perder el borrador porque Omni no lo persiste. Los eventos de metadatos del nuevo código se han probado en el editor original; la prueba real descrita arriba comprobó el mecanismo con el stream de salud existente, no un despliegue de 4.1.7.

## Cambios de 4.1.6

- Se comprobó que los dos selectores publicados en el Omni real solo ofrecían la fila vacía. El proveedor consultaba Resource Mappings, pero no el inventario físico de los nodos; no se afirma que el clúster carezca de mappings, porque la API también puede filtrarlos por permisos.
- PCI Device y USB Device incorporan el inventario `/nodes/{node}/hardware/pci` y `/hardware/usb`, con nombre, nodo y dirección en cada opción. Se mantienen los IDs de mappings existentes y la posibilidad de vaciar la selección.
- USB usa bus + ruta física del puerto. Se omiten hubs, dispositivos sin producto y entradas sin puerto válido. Si falta el nombre, se muestra fabricante/producto por ID.
- La selección física conserva el campo YAML `mapping` por compatibilidad con clases editadas en Omni, con valor `nodo/dirección (nombre)`. El proveedor extrae nodo y dirección y envía `host=...` a Proxmox. Un mapping con nombre sigue enviándose como `mapping=...`.
- Se comprueba que todos los dispositivos puedan estar en el mismo nodo, se rechazan duplicados y se revalida el inventario antes de crear o actualizar una VM. La descripción visible no determina la dirección física.
- Se retira Resource Affinity (PVE 9+) del formulario. Los valores antiguos se conservan al abrir y guardar.

El passthrough físico directo tiene una limitación de Proxmox: solo acepta autenticación root@pam con contraseña. Un token API debe utilizar Resource Mappings. El proveedor no crea mappings ni modifica permisos automáticamente. Los permisos de lectura del inventario y estas diferencias están explicados en las ayudas y en README.

La lista sigue siendo global porque el editor no admite filtrar sus opciones según otros controles. Cada dispositivo físico identifica su nodo y el esquema valida Target Node / Node Group, sin borrar una selección incompatible.

## Cambios de 4.1.5

- Add en Additional Disks crea cada disco con `disk_size: 20`. Antes, el control mostraba 0 aunque la propiedad no existía, y `required` rechazaba el objeto. Se añade el valor predeterminado del elemento y se retira esa obligación: tamaño omitido, null o 0 usa 20 GiB también en clases antiguas.
- Elegir `iso` o vaciar Provisioning Mode tiene prioridad sobre una plantilla guardada. Antes, tener `template_vmid` bastaba para seguir clonando. Los campos se conservan para volver a clone; el YAML antiguo sin `provisioning_mode` mantiene la inferencia de clonación.
- El esquema exige una plantilla solo al seleccionar clone. Cambiar el modo efectivo de una VM existente requiere reaprovisionar, también al vaciar un modo antes implícito.
- Se comprueba añadir dos discos, editar el primero sin perderlo y alternar clone/iso/vacío conservando la plantilla y Full Clone.

La visibilidad condicional solicitada no está implementada: el editor original construye una lista estática de controles y no consume reglas de visibilidad del proveedor. `if`/`then` permite validar, pero no ocultar. El control de variantes disponible sustituye datos al cambiar de variante, por lo que no se usa para ocultar opciones conservándolas.

## Cambios de 4.1.4

- Storage sustituye a Storage Selector (CEL) en el formulario del disco principal y de los discos adicionales.
- Se retiran All Disks, All NICs, VM Firewall Options y Storage Replication (ZFS). El backend sigue aceptando esas configuraciones guardadas; no se borran al abrir o guardar el formulario.
- Cores, Memory y Size llevan títulos cortos. Las unidades y valores predeterminados permanecen en las ayudas.
- PCI Mapping y USB Mapping son selectores vaciables con mappings asociados a nodos actuales. Se descartan mappings sin nodo válido, incluidos los que solo apuntan a nodos eliminados. Añadir un dispositivo no preselecciona hardware.
- La ayuda indica en qué nodos está definido cada mapping. Si no se descubren mappings, explica dónde configurarlos y cómo actualizar el inventario.
- El esquema valida Target Node, Node Group y la existencia de un nodo compatible con todos los dispositivos a la vez. Dos mappings que existen en nodos distintos sin ningún host común no forman una combinación válida.
- Los errores al consultar mappings ya no se ocultan detrás de un genérico «no hay nodos»: aparecen en los logs de descubrimiento y en los errores de aprovisionamiento.

El editor actual de Omni no vuelve a calcular las opciones de un selector a partir de otro campo. La lista visible sigue siendo global, y las condiciones validan la combinación; no se presenta como implementado el filtrado visual dependiente. Se ha comprobado que cambiar Target Node conserva los dispositivos elegidos, incluso si ahora son incompatibles, para que el usuario pueda corregirlos sin perderlos.

Una clase antigua que use `disks`/`nics` conserva esos overrides y debe editarse mediante YAML. Un CEL antiguo sigue aplicándose cuando Storage está vacío; elegir Storage explícitamente lo sustituye para ese disco. No se migran ni reescriben clases reales automáticamente.

## Cambios de 4.1.3

- Se retiran del formulario Workload Profiles y Network Bonds. El backend conserva la lectura del YAML antiguo para evitar cambiar silenciosamente la configuración de clases existentes. Los bonds antiguos eran interfaces dentro de Talos, no cambios de red del host Proxmox.
- Cada selector incluye una primera fila vacía para deseleccionar. Se guarda una cadena vacía real, no `auto` ni un espacio. Los títulos de las demás opciones coinciden con su valor para que Omni muestre correctamente selecciones guardadas.
- Boot Disk y Primary NIC tienen encabezados independientes; también se ordenan CPU, placement, aprovisionamiento y firmware. Son separadores visuales sobre los campos planos originales, sin migrar datos ni crear otra copia de los dispositivos.
- Se retira `legacy format` de las etiquetas. Additional Disks/NICs añaden dispositivos; All Disks/NICs sustituyen la configuración principal, como indican sus ayudas.
- Se elimina el publicador periódico del esquema. `--schema-refresh-interval` se acepta por compatibilidad pero no activa ningún temporizador. La topología para los desplegables se consulta una vez al arrancar; para actualizarla hay que reiniciar el proveedor.
- Los discos adicionales ya no exigen una expresión CEL cuando se elige un almacenamiento o se hereda el del disco principal.
- Las entradas vaciadas en listas de reglas/nodos HA y grupos de firewall se omiten antes de llamar a Proxmox.

## Límites comprobados del Omni alojado

El editor original usa grupos estáticos para objetos. No expone un acordeón desde el esquema del proveedor. El selector de variantes que sí incluye JSONForms reinicializa datos al cambiar de variante, por lo que no sirve para plegar una sección conservando sus ajustes. En 4.1.4 se ha retirado VM Firewall Options del formulario.

El código original de Omni vuelve a asignar el formulario al recibir otra vez el recurso MachineClass tras reconectar. La eliminación del temporizador de esquema no solucionaba los cierres por inactividad observados. El latido de 4.1.7 mantiene tráfico en la conexión para mitigarlos; no modifica cómo Omni trata el borrador después de una desconexión real.

La prueba de diagnóstico `hack/test/omni-ui/machine-class-reconnect.spec.ts` reproduce esa pérdida en la página MachineClass completa de Omni v1.13.3, sin modificar sus componentes. Usa sus implementaciones reales de MachineTemplate, JsonForm, useResourceWatch y Stream, con una API HTTP simulada:

- Una actualización del estado/esquema del provider conserva Cores = 8 en el formulario.
- Diez latidos de metadatos conservan el mismo control, el foco, Cores = 8 y un disco adicional de 64 GiB sin guardar.
- Tras cortar el stream de MachineClasses, la reconexión devuelve exactamente la misma clase guardada, con la misma versión y Cores = 2. El editor reemplaza el 8 sin guardar por el 2. No hay ninguna escritura a la API ni cambio en el estado/esquema del provider.

La prueba de reconexión documenta el fallo que permanece ante un corte real. Las pruebas anteriores sobre JsonForm por sí solo no cubrían el watcher de la página MachineClass. El provider no recibe el borrador del navegador y su esquema no puede cambiar este watcher ni persistir datos locales. La reproducción no atribuye el cierre de la conexión a la red del usuario, a un proxy ni al SaaS.

Las listas nuevas sustituyen `auto` por la selección vacía. El backend sigue aceptando los sentinelas antiguos en YAML; si una clase antigua contiene un sentinel que el desplegable ya no ofrece, elegir la fila vacía lo reemplaza al guardar. Los valores reales como cache `none` o VGA `none` siguen siendo opciones válidas.

## Correcciones conservadas de 4.1.2

- `/nodes` determina la pertenencia actual; referencias antiguas en grupos, HA, mappings o almacenamiento no añaden nodos.
- Solo almacenamiento activo, habilitado y con `images` admite discos de VM; `rootdir` por sí solo no es suficiente. Se valida también al aprovisionar.
- Los controles de CPU, RAM, disco y NIC usan sus campos planos guardados. Los límites numéricos están dentro de subesquemas para evitar el NaN que genera el renderizador de Omni al abrir un número omitido.
- El backend mantiene firmware, CPU, QoS, IPs, DNS, HA, replicación, firewall, PCI/USB y templates. Las opciones retiradas de la interfaz en 4.1.4 siguen disponibles por YAML.

El YAML agrupado de v4 sigue siendo válido en el backend, pero sus valores solo agrupados no aparecen en los controles planos; para esas clases debe editarse el manifiesto con `omnictl`.

## Verificación

- Pruebas Go de inventario con un nodo eliminado aún presente en recursos agregados, almacenamiento, grupos y mapeos.
- API simulada con un destino válido, uno `rootdir`, uno deshabilitado y otro inactivo.
- Detector de carreras, `go vet`, lint completo y compilación Linux amd64/arm64.
- Once pruebas de compatibilidad con el renderizador original de Omni v1.13.3, commit `1834114db70c2bbfff71cd4ba8a1882a01504193`, sin modificar sus componentes: datos antiguos conservados, formulario nuevo sin NaN, edición durante actualización de esquema, selectores vaciables, campos eliminados y títulos cortos, opciones PCI/USB físicas y mappings, persistencia de sus etiquetas, validación de combinaciones de nodo/grupo/dispositivos, creación y edición de discos, valores predeterminados y cambios de modo de aprovisionamiento.
- Prueba visual local usando ese editor original y datos sintéticos; no se modificó ninguna clase ni VM real.
- Tres pruebas adicionales de la página completa: actualización de esquema, latidos de metadatos y reconexión. Pruebas Go con reloj virtual para emisión de eventos, conservación de recursos, arranque, destrucción, cancelación, desactivación y recuperación tras timeout.

Para repetir las pruebas, generar desde la raíz del proveedor un esquema sintético con `go run hack/test/omni-ui/schema_fixture.go > /tmp/provider-schema.json`. En un checkout de Omni de esa revisión con sus dependencias de frontend instaladas, copiar `hack/test/omni-ui/provider-schema.spec.ts` a `frontend/src/components/Form/ProviderSchema.spec.ts` y ejecutar desde `frontend`:

```sh
PROXMOX_PROVIDER_SCHEMA=/tmp/provider-schema.json \
  npm test -- --run src/components/Form/ProviderSchema.spec.ts
```

Ese archivo es solo una prueba: no es un parche del producto ni debe incluirse en un build de Omni.
Las pruebas reales de Proxmox siguen limitadas a lectura y simulación de API; no se ha hecho E2E con creación de VMs en el clúster del usuario.

Para reproducir el diagnóstico de reconexión, copiar `hack/test/omni-ui/machine-class-reconnect.spec.ts` a `frontend/src/views/MachineClasses/ProviderReconnect.spec.ts` del mismo checkout original y ejecutar desde `frontend`:

```sh
PROXMOX_PROVIDER_SCHEMA=/tmp/provider-schema.json \
  npm test -- --run src/views/MachineClasses/ProviderReconnect.spec.ts
```

Sus tres pruebas deben pasar en esa revisión: la de reconexión comprueba explícitamente la pérdida del borrador. No debe interpretarse un resultado verde como que Omni conserva los cambios después de un corte real de red.
