# Correcciones del editor e inventario: 4.1.5

La solución modifica exclusivamente el proveedor. No incluye parches, extensiones de navegador ni cambios al Omni alojado.

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

En la sesión real se observaron errores de red simultáneos en los streams de MachineClasses, InfraProviderStatuses, métricas y notificaciones. El código original de Omni vuelve a asignar el formulario al recibir otra vez el recurso MachineClass tras reconectar. El proveedor no controla esa conexión navegador-SaaS. La eliminación de su temporizador quita publicaciones periódicas de esquema, pero no arregla por sí sola esas reconexiones ni una recarga manual. No se presenta como resuelta esa pérdida de borradores.

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
- Diez pruebas de compatibilidad con el renderizador original de Omni v1.13.3, commit `1834114db70c2bbfff71cd4ba8a1882a01504193`, sin modificar sus componentes: datos antiguos conservados, formulario nuevo sin NaN, edición durante actualización de esquema, selectores vaciables, campos eliminados y títulos cortos, opciones PCI/USB y validación de combinaciones de nodo/grupo/mappings, creación y edición de discos, valores predeterminados y cambios de modo de aprovisionamiento.
- Prueba visual local usando ese editor original y datos sintéticos; no se modificó ninguna clase ni VM real.

Para repetir las pruebas, generar desde la raíz del proveedor un esquema sintético con `go run hack/test/omni-ui/schema_fixture.go > /tmp/provider-schema.json`. En un checkout de Omni de esa revisión con sus dependencias de frontend instaladas, copiar `hack/test/omni-ui/provider-schema.spec.ts` a `frontend/src/components/Form/ProviderSchema.spec.ts` y ejecutar desde `frontend`:

```sh
PROXMOX_PROVIDER_SCHEMA=/tmp/provider-schema.json \
  npm test -- --run src/components/Form/ProviderSchema.spec.ts
```

Ese archivo es solo una prueba: no es un parche del producto ni debe incluirse en un build de Omni.
Las pruebas reales de Proxmox siguen limitadas a lectura y simulación de API; no se ha hecho E2E con creación de VMs en el clúster del usuario.
